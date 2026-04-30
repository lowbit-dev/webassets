# webassets

A fast, flexible Go package for serving static web assets with in-memory caching, on-the-fly minification, and virtual bundle support.

## Features
- **In-memory caching**: Serve assets from memory for maximum performance.
- **On-the-fly minification**: Minify JavaScript and CSS using [tdewolff/minify](https://github.com/tdewolff/minify).
- **Virtual bundles**: Concatenate and serve multiple files as a single asset (e.g., `app.bundle.js`).
- **Singleflight protection**: Prevents duplicate work under concurrent load.
- **Customizable error logging**: Plug in your own logger for visibility into handled errors.
- **Cache exclusion**: Exclude specific URL prefixes from caching.
- **Secure by default**: Files prefixed with `_` are not served.

## Installation

```
go get github.com/cornejong/webassets
```

## Usage

```go
import (
    "net/http"
    "github.com/cornejong/webassets"
)

func main() {
    h := webassets.New("./web/static",
        webassets.WithCache(true),
        webassets.WithMinification(true),
        webassets.WithBundle("app.bundle.js", "js/vendor.js", "js/app.js"),
        webassets.WithErrorLogger(func(err error) { log.Println("webassets error:", err) }),
    )
    http.Handle("/assets/", http.StripPrefix("/assets", h))
    http.ListenAndServe(":8080", nil)
}
```

## Options
- `WithCache(enabled bool)`: Enable or disable in-memory caching.
- `WithCacheExclude(prefixes ...string)`: Exclude URL prefixes from caching.
- `WithMinification(enabled bool)`: Enable or disable JS/CSS minification.
- `WithBundle(name string, files ...string)`: Register a virtual bundle.
- `WithErrorLogger(fn func(error))`: Set a custom error logger.

## Bundles
Bundles are virtual files that concatenate multiple source files. Example:

```go
webassets.WithBundle("app.bundle.js", "js/vendor.js", "js/app.js")
```

Requesting `/assets/app.bundle.js` will serve the concatenated and minified result.

## Security
- Files prefixed with `_` (e.g., `_secret.js`) are not served.
- Only text-based files are minified and (future) compressed.

## Roadmap
- [ ] Transparent gzip/brotli compression with content negotiation
- [ ] ETag/Last-Modified support for bundles
- [ ] Live reload/dev mode helpers

## License
MIT
