package upload

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrInvalidArchive = errors.New("invalid archive")

	ErrTooManyEntries = errors.New("too many entries")

	ErrTooLarge = errors.New("upload too large")

	ErrNoIndexHTML = errors.New("upload contains no index.html")
)

const prefixSegmentBytes = 8

func newPrefix() string {
	var b [prefixSegmentBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("upload: crypto/rand unavailable: %v", err))
	}
	return "uploads/" + hex.EncodeToString(b[:]) + "/"
}

// contentTypes maps a file extension to the Content-Type stored objects are
// served with.
//
// Garage does not preserve content-type metadata on upload, so it is derived
// from the key. An unknown extension returns "", which combined with
// X-Content-Type-Options: nosniff stops a browser from guessing.
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

// immutableNames are never served with a long cache lifetime regardless of
// extension. An index can be rewritten under the same name on every deploy,
// and serving a stale one is the classic "I deployed and nothing changed"
// bug.
var immutableNames = map[string]bool{
	"index.html": true,
	"index.htm":  true,
}

func ContentTypeFor(key string) string {
	if ct, ok := contentTypes[strings.ToLower(extension(key))]; ok {
		return ct
	}
	return ""
}

// IsImmutable reports whether a key may be cached forever.
//
// Only true for content-addressed names: a hash segment under assets/, or a
// bundle named app.<hash>.js. Everything else must be revalidated, because
// its bytes can change without its address changing.
func IsImmutable(key string) bool {
	base := key
	if i := strings.LastIndex(key, "/"); i >= 0 {
		base = key[i+1:]
	}
	if immutableNames[strings.ToLower(base)] {
		return false
	}
	if strings.HasPrefix(key, "assets/") {
		return true
	}
	return isHashed(base)
}

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

func extension(key string) string {
	base := key
	if i := strings.LastIndex(key, "/"); i >= 0 {
		base = key[i+1:]
	}
	if i := strings.LastIndex(base, "."); i >= 0 {
		return base[i:]
	}
	return ""
}
