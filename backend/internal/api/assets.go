package api

import (
	"path"
	"strings"
)

// contentTypes maps a file extension to the Content-Type the data plane
// responds with.
//
// This is needed on the serving side rather than the upload side. Objects are
// written without a Content-Type, so Garage records application/octet-stream
// for everything, and serving that would make a browser download index.html
// instead of rendering it. The header belongs to whoever writes the HTTP
// response, which is here.
//
// It is an explicit table rather than mime.TypeByExtension because the latter
// consults the OS mime database, so the same key would resolve differently on
// a full distro and on a scratch container.
var contentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".htm":   "text/html; charset=utf-8",
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
	".avif":  "image/avif",
	".ico":   "image/x-icon",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".otf":   "font/otf",
	".txt":   "text/plain; charset=utf-8",
	".xml":   "application/xml",
	".wasm":  "application/wasm",
	".pdf":   "application/pdf",
}

// contentTypeFor returns the Content-Type for a key, or "" if unknown.
func contentTypeFor(key string) string {
	return contentTypes[strings.ToLower(path.Ext(key))]
}

// neverImmutable are revalidated even though their extension might look
// content-addressed. An index can be rewritten under the same name on every
// deploy, and serving a stale one is the classic "I deployed and nothing
// changed" bug.
var neverImmutable = map[string]bool{
	"index.html": true,
	"index.htm":  true,
}

// IsImmutable reports whether a key may be cached forever.
//
// True only for content-addressed names: a path under assets/, or a bundle
// named app.<hash>.ext. Everything else must be revalidated, because its
// bytes can change without its address changing.
func IsImmutable(key string) bool {
	base := key
	if i := strings.LastIndex(key, "/"); i >= 0 {
		base = key[i+1:]
	}
	if neverImmutable[strings.ToLower(base)] {
		return false
	}
	if strings.HasPrefix(key, "assets/") {
		return true
	}
	return isHashed(base)
}

// isHashed recognises the bundler naming convention name.<hash>.ext.
func isHashed(base string) bool {
	dot := strings.LastIndex(base, ".")
	if dot <= 0 {
		return false
	}
	rest := base[:dot]
	hash := rest[strings.LastIndex(rest, ".")+1:]
	if len(hash) < 8 {
		return false
	}
	for _, r := range hash {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}
