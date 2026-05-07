package webassets

import (
	"bytes"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/tdewolff/minify/v2"
	"github.com/tdewolff/minify/v2/css"
	"github.com/tdewolff/minify/v2/js"
)

// TODO(compression): extend cacheEntry to store pre-compressed variants alongside the raw

// bytes, e.g. `gzip []byte` and `br []byte`. Compression should happen once, inside
// buildFileEntry/buildBundleEntry, right after minification — so the work is paid at
// warm-up time rather than per request. The singleflight guard already ensures this only
// happens once per resource even under concurrent load.
//
// At serve time, inspect the Accept-Encoding request header (in order of preference):
//  1. br  (Brotli)  — best ratio; use golang.org/x/net/http2 or github.com/andybalholm/brotli
//  2. gzip          — widest support; use compress/gzip from stdlib
//  3. identity      — fall back to the raw bytes already in the entry
//
// Set the Content-Encoding response header accordingly and omit Content-Length
// (the compressed size differs from the raw size). Also add a Vary: Accept-Encoding
// header so that CDNs and reverse proxies cache the compressed and uncompressed
// variants separately.
//
// Only compress text-based content types (JS, CSS, HTML, JSON, SVG, XML).
// Binary formats (images, fonts, woff2) are already compressed and re-compressing
// them wastes CPU for no gain.
type cacheEntry struct {
	data        []byte
	contentType string
}

type bundle struct {
	files []string
}

// Handler serves static web assets from a directory.
// It supports in-memory caching, on-the-fly JS/CSS minification,
// and virtual bundles that concatenate multiple source files.
//
// Usage:
//
//	h := webassets.New("./web/static",
//	    webassets.WithCache(),
//	    webassets.WithMinification(),
//	    webassets.WithBundle("app.bundle.js", "js/vendor.js", "js/app.js"),
//	)
//	mux.Handle("/assets/", http.StripPrefix("/assets", h))
type Handler struct {
	dir          string
	useCache     bool
	cacheExclude []string
	useMinify    bool
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
	h := &Handler{
		dir:         dir,
		bundles:     make(map[string]bundle),
		fileCache:   make(map[string]cacheEntry),
		bundleCache: make(map[string]cacheEntry),
		fileServer:  http.FileServer(http.Dir(dir)),
	}
	for _, opt := range opts {
		opt(h)
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

	w.Header().Set("Content-Type", entry.contentType)
	// TODO(compression): negotiate Content-Encoding here before writing (see cacheEntry TODO).
	w.Write(entry.data)
}

func (h *Handler) serveBundle(w http.ResponseWriter, r *http.Request, name string, b bundle) {
	if h.useCache {
		h.mu.RLock()
		entry, ok := h.bundleCache[name]
		h.mu.RUnlock()
		if ok {
			w.Header().Set("Content-Type", entry.contentType)
			w.Write(entry.data)
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

	entry := result.(cacheEntry)
	w.Header().Set("Content-Type", entry.contentType)
	// TODO(compression): negotiate Content-Encoding here before writing (see cacheEntry TODO).
	w.Write(entry.data)
}

func (h *Handler) buildFileEntry(name string) (cacheEntry, error) {
	filePath := filepath.Join(h.dir, filepath.FromSlash(name))
	data, err := os.ReadFile(filePath)
	if err != nil {
		return cacheEntry{}, err
	}

	ct := contentTypeFor(name)
	return cacheEntry{data: h.minifyIfEnabled(ct, data), contentType: ct}, nil
}

func (h *Handler) buildBundleEntry(name string, b bundle) (cacheEntry, error) {
	var buf bytes.Buffer
	for _, f := range b.files {
		data, err := os.ReadFile(filepath.Join(h.dir, filepath.FromSlash(f)))
		if err != nil {
			return cacheEntry{}, err
		}

		buf.Write(data)
		buf.WriteByte('\n')
	}

	ct := contentTypeFor(name)

	return cacheEntry{data: h.minifyIfEnabled(ct, buf.Bytes()), contentType: ct}, nil
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
