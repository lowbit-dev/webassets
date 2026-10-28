# webassets

A fast, flexible Go package for serving static web assets with in-memory caching, on-the-fly minification, and virtual bundle support.

## Features
- **In-memory caching**: Serve assets from memory for maximum performance.
- **On-the-fly minification**: Minify JavaScript and CSS using [tdewolff/minify](https://github.com/tdewolff/minify).
- **gzip and Brotli compression**: Text assets are compressed once when cached and served with the best encoding the client accepts.
- **Virtual bundles**: Concatenate and serve multiple files as a single asset (e.g., `app.bundle.js`).
- **Singleflight protection**: Prevents duplicate work under concurrent load.
- **Customizable error logging**: Plug in your own logger for visibility into handled errors.
- **Any `fs.FS`**: Serve from a directory or an `fs.FS` such as `embed.FS`.
- **Cache exclusion**: Exclude specific URL prefixes from caching.
- **Secure by default**: Files prefixed with `_` are not served.

## Installation

```
go get lowbit.dev/webassets
```

## Usage

```go
import (
    "net/http"
    "lowbit.dev/webassets"
)

func main() {
    h := webassets.New("./web/static",
        webassets.WithCache(true),
        webassets.WithMinification(true),
        webassets.WithCompression(true),
        webassets.WithBundle("app.bundle.js", "js/vendor.js", "js/app.js"),
        webassets.WithErrorLogger(func(err error) { log.Println("webassets error:", err) }),
    )
    http.Handle("/assets/", http.StripPrefix("/assets", h))
    http.ListenAndServe(":8080", nil)
}
```

### Serving from an `fs.FS`

Use `NewFromFS` to serve assets from any `fs.FS`, such as an `embed.FS`. `New(dir)` is equivalent to `NewFromFS(os.DirFS(dir))`.

```go
//go:embed static
var static embed.FS

func main() {
    sub, _ := fs.Sub(static, "static") // strip the "static/" prefix
    h := webassets.NewFromFS(sub, webassets.WithCache(true))
    http.Handle("/assets/", http.StripPrefix("/assets", h))
    http.ListenAndServe(":8080", nil)
}
```

## Options
- `WithCache(enabled bool)`: Enable or disable in-memory caching.
- `WithCacheExclude(prefixes ...string)`: Exclude URL prefixes from caching.
- `WithMinification(enabled bool)`: Enable or disable JS/CSS minification.
- `WithCompression(enabled bool)`: Enable or disable gzip/Brotli compression (requires `WithCache(true)`).
- `WithCompressionLevels(gzipLevel, brotliLevel int)`: Set compression levels (gzip 1–9, Brotli 0–11). Defaults to the best compression for both; out-of-range values fall back to the default.
- `WithBundle(name string, files ...string)`: Register a virtual bundle.
- `WithErrorLogger(fn func(error))`: Set a custom error logger.

## Bundles
Bundles are virtual files that concatenate multiple source files. Example:

```go
webassets.WithBundle("app.bundle.js", "js/vendor.js", "js/app.js")
```

Requesting `/assets/app.bundle.js` will serve the concatenated and minified result.

## Compression
With `WithCache(true)` and `WithCompression(true)`, text-based assets (JS, CSS, HTML, JSON, SVG, XML) of at least 1 KB are compressed with gzip and Brotli the first time they are requested. The result is cached, so the compression cost is paid only once per asset. Each request gets the best encoding allowed by its `Accept-Encoding` header (Brotli, then gzip, then uncompressed), with `Content-Encoding` and `Vary: Accept-Encoding` set accordingly.

Images and fonts are not compressed because they are already compressed. When caching is disabled (e.g. during development), nothing is compressed.

Higher levels give smaller files but take longer on first request. For large bundles, consider a lower Brotli level:

```go
webassets.WithCompressionLevels(gzip.BestCompression, 6)
```

## Security
- Files prefixed with `_` (e.g., `_secret.js`) are not served.
- Only text-based files are minified and compressed.

## Roadmap
- [ ] ETag/Last-Modified support for bundles
- [ ] Live reload/dev mode helpers

## License
MIT
