package api

import (
	"path"
	"strings"
)

// contentTypes maps a file extension to the Content-Type served with it.
//
// Garage does not preserve content-type metadata on upload, so the type is
// derived here. An unknown extension is served with no Content-Type header,
// which combined with nosniff means browsers will not guess.
var contentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".json":  "application/json",
	".map":   "application/json",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".webp":  "image/webp",
	".ico":   "image/x-icon",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".txt":   "text/plain; charset=utf-8",
	".xml":   "application/xml",
	".wasm":  "application/wasm",
}

func contentTypeFor(key string) string {
	return contentTypes[strings.ToLower(path.Ext(key))]
}
