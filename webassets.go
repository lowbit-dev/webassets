package webassets

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
	"github.com/tdewolff/minify/v2"
	"github.com/tdewolff/minify/v2/css"
	"github.com/tdewolff/minify/v2/js"
)

// minCompressSize is the smallest payload worth compressing. Below this the
// encoding overhead outweighs the savings.
const minCompressSize = 1024

// cacheEntry holds a built asset. The gzip and br variants are compressed once
// at build time and are nil when compression is disabled, the content type is
// not compressible, or the compressed result is not smaller than data.
type cacheEntry struct {
	data        []byte
	gzip        []byte
	br          []byte
	contentType string
}

type bundle struct {
	files []string
}

// Handler serves static web assets from a directory or an fs.FS.
// It supports in-memory caching, on-the-fly JS/CSS minification,
// and virtual bundles that concatenate multiple source files.
//
// Usage:
//
//	h := webassets.New("./web/static",
//	    webassets.WithCache(true),
//	    webassets.WithMinification(true),
//	    webassets.WithCompression(true),
//	    webassets.WithBundle("app.bundle.js", "js/vendor.js", "js/app.js"),
//	)
//	mux.Handle("/assets/", http.StripPrefix("/assets", h))
//
// Use NewFromFS to serve from an fs.FS instead, such as an embed.FS.
type Handler struct {
	fsys         fs.FS
	useCache     bool
	cacheExclude []string
	useMinify    bool
	useCompress  bool
	gzipLevel    int
	brotliLevel  int
	bundles      map[string]bundle

	mu          sync.RWMutex
	fileCache   map[string]cacheEntry
	bundleCache map[string]cacheEntry
	minifier    *minify.M
	fileServer  http.Handler
	sf          sfGroup
	errLogger   func(error)
}

// Option configures a Handler.
type Option func(*Handler)

// WithCache enables in-memory caching. Each asset is read from disk on the
// first request and served from memory on all subsequent requests.
func WithCache(enabled bool) Option {
	return func(h *Handler) { h.useCache = enabled }
}

// WithCacheExclude prevents paths with the given URL prefixes from being
// cached. Only applies when WithCache is also used.
func WithCacheExclude(prefixes ...string) Option {
	return func(h *Handler) {
		h.cacheExclude = append(h.cacheExclude, prefixes...)
	}
}

// WithMinification enables on-the-fly minification of JavaScript and CSS.
// Minification errors are silently ignored and the original content is served.
func WithMinification(enabled bool) Option {
	return func(h *Handler) { h.useMinify = enabled }
}

// WithCompression enables gzip and Brotli compression of text-based assets
// (JS, CSS, HTML, JSON, SVG, XML). Each asset is compressed once when it is
// cached and the best encoding the client accepts is served. Only applies when
// WithCache is also enabled; uncached responses are never compressed.
func WithCompression(enabled bool) Option {
	return func(h *Handler) { h.useCompress = enabled }
}

// WithCompressionLevels sets the gzip (1-9) and Brotli (0-11) compression
// levels. Higher levels compress better but take longer when an asset is first
// cached. An out-of-range level falls back to its default
// (gzip.BestCompression, brotli.BestCompression) and is reported to the error
// logger.
func WithCompressionLevels(gzipLevel, brotliLevel int) Option {
	return func(h *Handler) {
		h.gzipLevel = gzipLevel
		h.brotliLevel = brotliLevel
	}
}

// WithErrorLogger sets a function that is called whenever an error is handled
// gracefully (e.g. a missing file, a failed minification). The handler always
// recovers and returns a safe HTTP response; this just gives you visibility.
// Example: webassets.WithErrorLogger(func(err error) { slog.Error(err.Error()) })
func WithErrorLogger(fn func(error)) Option {
	return func(h *Handler) { h.errLogger = fn }
}

// WithBundle registers a virtual bundle file accessible at /<name>. When
// requested, the source files are read from the handler root directory,
// concatenated in order, and optionally minified.
func WithBundle(name string, files ...string) Option {
	return func(h *Handler) {
		h.bundles[name] = bundle{files: files}
	}
}

// New creates a Handler that serves files from dir.
func New(dir string, opts ...Option) *Handler {
	return NewFromFS(os.DirFS(dir), opts...)
}

// NewFromFS creates a Handler that serves files from fsys. This allows serving
// assets from an embed.FS or any other fs.FS implementation. Use fs.Sub to
// strip a leading directory, e.g. when embedding "static/*":
//
//	//go:embed static
//	var static embed.FS
//
//	sub, _ := fs.Sub(static, "static")
//	h := webassets.NewFromFS(sub, webassets.WithCache(true))
func NewFromFS(fsys fs.FS, opts ...Option) *Handler {
	h := &Handler{
		fsys:        fsys,
		bundles:     make(map[string]bundle),
		fileCache:   make(map[string]cacheEntry),
		bundleCache: make(map[string]cacheEntry),
		fileServer:  http.FileServerFS(fsys),
		gzipLevel:   gzip.BestCompression,
		brotliLevel: brotli.BestCompression,
	}

	for _, opt := range opts {
		opt(h)
	}

	if h.gzipLevel < gzip.BestSpeed || h.gzipLevel > gzip.BestCompression {
		h.logError(fmt.Errorf("webassets: invalid gzip level %d, using %d", h.gzipLevel, gzip.BestCompression))
		h.gzipLevel = gzip.BestCompression
	}

	if h.brotliLevel < brotli.BestSpeed || h.brotliLevel > brotli.BestCompression {
		h.logError(fmt.Errorf("webassets: invalid brotli level %d, using %d", h.brotliLevel, brotli.BestCompression))
		h.brotliLevel = brotli.BestCompression
	}

	if h.useMinify {
		m := minify.New()
		m.AddFunc("text/css", css.Minify)
		m.AddFuncRegexp(regexp.MustCompile(`^(application|text)/(x-)?(java|ecma)script$`), js.Minify)
		h.minifier = m
	}

	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")

	// Bundles are virtual files; check them before falling through to disk.
	if b, ok := h.bundles[name]; ok {
		h.serveBundle(w, r, name, b)
		return
	}

	// TODO: If the filename is prefixed with "_" then we by default are going to consider that an internal only file. not available through the webassets endpoint
	if strings.HasPrefix(filepath.Base(r.URL.Path), "_") {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	// Dev mode or excluded path: delegate to the standard file server so that
	// disk changes are reflected immediately and all HTTP semantics (ETags,
	// Range, Last-Modified) are handled automatically.
	if !h.useCache || h.isExcluded(r.URL.Path) {
		h.fileServer.ServeHTTP(w, r)
		return
	}

	h.serveFile(w, r, name)
}

func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request, name string) {
	h.mu.RLock()
	entry, ok := h.fileCache[name]
	h.mu.RUnlock()

	if !ok {
		result, err, _ := h.sf.Do("f:"+name, func() (any, error) {
			e, err := h.buildFileEntry(name)
			if err != nil {
				return cacheEntry{}, err
			}
			h.mu.Lock()
			h.fileCache[name] = e
			h.mu.Unlock()
			return e, nil
		})

		if err != nil {
			h.logError(err)
			http.NotFound(w, r)
			return
		}

		entry = result.(cacheEntry)
	}

	writeEntry(w, r, entry)
}

func (h *Handler) serveBundle(w http.ResponseWriter, r *http.Request, name string, b bundle) {
	if h.useCache {
		h.mu.RLock()
		entry, ok := h.bundleCache[name]
		h.mu.RUnlock()
		if ok {
			writeEntry(w, r, entry)
			return
		}
	}

	result, err, _ := h.sf.Do("b:"+name, func() (any, error) {
		e, err := h.buildBundleEntry(name, b)
		if err != nil {
			return cacheEntry{}, err
		}
		if h.useCache {
			h.mu.Lock()
			h.bundleCache[name] = e
			h.mu.Unlock()
		}

		return e, nil
	})

	if err != nil {
		h.logError(err)
		http.Error(w, "bundle build failed", http.StatusInternalServerError)
		return
	}

	writeEntry(w, r, result.(cacheEntry))
}

// writeEntry writes entry using the best encoding the client accepts.
func writeEntry(w http.ResponseWriter, r *http.Request, entry cacheEntry) {
	data := entry.data
	if entry.gzip != nil || entry.br != nil {
		// The response differs per Accept-Encoding, so shared caches must
		// store the variants separately.
		w.Header().Add("Vary", "Accept-Encoding")

		brQ, gzipQ := acceptEncodingQ(r.Header.Get("Accept-Encoding"))
		if entry.br == nil {
			brQ = 0
		}
		if entry.gzip == nil {
			gzipQ = 0
		}

		switch {
		case brQ > 0 && brQ >= gzipQ:
			w.Header().Set("Content-Encoding", "br")
			data = entry.br
		case gzipQ > 0:
			w.Header().Set("Content-Encoding", "gzip")
			data = entry.gzip
		}
	}

	w.Header().Set("Content-Type", entry.contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}

func (h *Handler) buildFileEntry(name string) (cacheEntry, error) {
	data, err := fs.ReadFile(h.fsys, name)
	if err != nil {
		return cacheEntry{}, err
	}

	ct := contentTypeFor(name)
	e := cacheEntry{data: h.minifyIfEnabled(ct, data), contentType: ct}
	h.compressIfEnabled(&e)

	return e, nil
}

func (h *Handler) buildBundleEntry(name string, b bundle) (cacheEntry, error) {
	var buf bytes.Buffer
	for _, f := range b.files {
		data, err := fs.ReadFile(h.fsys, strings.TrimPrefix(path.Clean("/"+f), "/"))
		if err != nil {
			return cacheEntry{}, err
		}

		buf.Write(data)
		buf.WriteByte('\n')
	}

	ct := contentTypeFor(name)
	e := cacheEntry{data: h.minifyIfEnabled(ct, buf.Bytes()), contentType: ct}
	h.compressIfEnabled(&e)

	return e, nil
}

func (h *Handler) minifyIfEnabled(contentType string, data []byte) []byte {
	if !h.useMinify || h.minifier == nil {
		return data
	}

	mediatype := strings.SplitN(contentType, ";", 2)[0]
	minified, err := h.minifier.Bytes(strings.TrimSpace(mediatype), data)
	if err != nil {
		h.logError(err)
		return data
	}

	return minified
}

// compressIfEnabled fills the gzip and br variants of e. Compression only
// happens for cached entries, so the cost is paid once per asset. A variant is
// kept only if it is smaller than the original; errors are logged and the
// variant is skipped.
func (h *Handler) compressIfEnabled(e *cacheEntry) {
	if !h.useCompress || !h.useCache || len(e.data) < minCompressSize || !isCompressible(e.contentType) {
		return
	}

	if gz, err := gzipBytes(e.data, h.gzipLevel); err != nil {
		h.logError(err)
	} else if len(gz) < len(e.data) {
		e.gzip = gz
	}

	if br, err := brotliBytes(e.data, h.brotliLevel); err != nil {
		h.logError(err)
	} else if len(br) < len(e.data) {
		e.br = br
	}
}

func gzipBytes(data []byte, level int) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, level)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(data); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func brotliBytes(data []byte, level int) ([]byte, error) {
	var buf bytes.Buffer
	bw := brotli.NewWriterLevel(&buf, level)
	if _, err := bw.Write(data); err != nil {
		return nil, err
	}
	if err := bw.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// isCompressible reports whether contentType is a text-based type that
// benefits from compression. Images and fonts are already compressed.
func isCompressible(contentType string) bool {
	mediatype := strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0])
	switch mediatype {
	case "application/javascript", "application/json", "application/xml", "image/svg+xml":
		return true
	}

	return strings.HasPrefix(mediatype, "text/")
}

// acceptEncodingQ returns the quality values the client assigns to br and gzip
// in an Accept-Encoding header. Encodings that are not listed inherit the
// quality of "*"; a quality of 0 means not acceptable.
func acceptEncodingQ(header string) (brQ, gzipQ float64) {
	brQ, gzipQ, starQ := -1.0, -1.0, 0.0
	for _, part := range strings.Split(header, ",") {
		name, params, _ := strings.Cut(part, ";")
		q := 1.0
		for _, param := range strings.Split(params, ";") {
			key, value, ok := strings.Cut(param, "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(key), "q") {
				continue
			}
			f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil {
				f = 0
			}
			q = f
		}

		switch strings.ToLower(strings.TrimSpace(name)) {
		case "br":
			brQ = q
		case "gzip", "x-gzip":
			gzipQ = q
		case "*":
			starQ = q
		}
	}

	if brQ < 0 {
		brQ = starQ
	}
	if gzipQ < 0 {
		gzipQ = starQ
	}

	return brQ, gzipQ
}

func (h *Handler) logError(err error) {
	if h.errLogger != nil {
		h.errLogger(err)
	}
}

func (h *Handler) isExcluded(urlPath string) bool {
	for _, prefix := range h.cacheExclude {
		if strings.HasPrefix(urlPath, prefix) {
			return true
		}
	}

	return false
}

// contentTypeFor returns the MIME type for the given file name based on its
// extension. Common web asset types are hardcoded to avoid OS-level variance
// in mime databases; everything else falls back to mime.TypeByExtension.
func contentTypeFor(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".js", ".mjs":
		return "application/javascript"
	case ".css":
		return "text/css"
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".json":
		return "application/json"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".ico":
		return "image/x-icon"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".ttf":
		return "font/ttf"
	case ".txt":
		return "text/plain; charset=utf-8"
	default:
		if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
			return ct
		}
		return "application/octet-stream"
	}
}
