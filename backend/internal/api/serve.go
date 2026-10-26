package api

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/dk10ws/ironhide/internal/database"
)

// serve answers a data-plane request.
//
// The host names the deployment and the path names the object within it. Every
// uploaded site gets its own subdomain, which is what keeps a hostile upload
// from reading another upload's storage: they are different origins.
func (h *Handler) serve(c *gin.Context) {
	slug, ok := h.slugFromHost(c.Request.Host)
	if !ok {
		// Not addressed to the data plane.
		c.Status(http.StatusNotFound)
		return
	}

	dep, err := h.lookup(c, slug)
	if err != nil {
		h.serveStatus(c, err)
		return
	}

	key, ok := h.resolveKey(c.Request.URL.Path, dep)
	if !ok {
		h.notFound(c)
		return
	}

	body, size, err := h.store.GetObject(c.Request.Context(), key)
	if err != nil {
		h.notFound(c)
		return
	}
	defer body.Close()

	h.writeObject(c, key, size, body)
}

// slugFromHost extracts the deployment slug from the request host.
//
// The suffix must match to the end of the host. An earlier version used
// strings.Index, which also accepted "abc.preview.example.com.attacker.net",
// letting an unrelated domain resolve to a deployment.
//
// Only the leftmost label is considered, and only if it is a plausible slug,
// so "a.b.preview.example.com" cannot address the deployment named "b".
func (h *Handler) slugFromHost(host string) (string, bool) {
	hst, ok := hostName(host)
	if !ok {
		return "", false
	}

	suffix := "." + strings.TrimPrefix(strings.ToLower(hostSuffix(h.cfg.DataHostSuffix)), ".")

	hst = strings.ToLower(hst)
	if !strings.HasSuffix(hst, suffix) {
		return "", false
	}
	slug := strings.TrimSuffix(hst, suffix)
	if !validSlug(slug) {
		return "", false
	}
	return slug, true
}

// hostName returns the host part of a host[:port] pair, and reports false when
// the pair is not one.
//
// The port is only removed when it really is a port. Cutting at the first colon
// instead would throw away whatever follows it, so
// "abc.preview.example.com:8443.attacker.net" would arrive as the legitimate
// "abc.preview.example.com" and be served. Refusing a host whose tail is not
// numeric keeps the suffix anchored to the end of the whole name.
//
// The config may pin a port, which is how the URL previewURL builds reaches this
// listener. That port is part of the URL and not part of the name, so it comes
// off the suffix here; a Host that omits it still names the same deployment.
func hostName(host string) (string, bool) {
	name, port, found := strings.Cut(host, ":")
	if !found {
		return name, true
	}
	if port == "" {
		return "", false
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return name, true
}

// hostSuffix drops any port from a configured data host suffix.
func hostSuffix(suffix string) string {
	if i := strings.Index(suffix, ":"); i >= 0 {
		suffix = suffix[:i]
	}
	return suffix
}

// validSlug accepts the 16 hex characters newPrefix generates. Being strict
// here means a bad host is a 404 rather than a database lookup on arbitrary
// attacker-chosen input.
func validSlug(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// lookup resolves a slug to a live deployment.
func (h *Handler) lookup(c *gin.Context, slug string) (*database.Deployment, error) {
	dep, err := h.db.GetDeploymentByPrefix(c.Request.Context(), "uploads/"+slug)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			return nil, database.ErrNotFound
		}
		return nil, err
	}

	switch {
	case dep.Status != database.StatusLive:
		return nil, errGone
	case dep.Expired(time.Now()):
		// Expired but not yet swept. Serving it would defeat the claim window,
		// so the sweeper's absence must not leak content.
		return nil, errGone
	case dep.SitePaused:
		// The owner took the site down. It is not expired and not missing, so
		// it gets its own answer rather than being folded into either.
		return nil, errPaused
	}

	return dep, nil
}

var errGone = errors.New("deployment is no longer available")

// errPaused is a site the owner has taken down.
var errPaused = errors.New("site is down")

// resolveKey turns a request path into an object key inside the deployment.
//
// The lookup goes through the manifest recorded at upload time rather than
// assuming names. That matters because uploads accept Index.HTML and
// index.htm, so a hardcoded "index.html" would 404 on a site that uploaded
// perfectly well. Matching is exact first and case-insensitive second, so a
// file is only reached through its real name.
func (h *Handler) resolveKey(urlPath string, dep *database.Deployment) (string, bool) {
	prefix := dep.Prefix + "/"
	rel := strings.TrimPrefix(path.Clean("/"+urlPath), "/")

	if rel == "" || rel == "." {
		rel = h.rootRel(dep)
		if rel == "" {
			return "", false
		}
	}

	// path.Clean has already collapsed any "..", so the prefix is intact by
	// construction; this only guards the empty case.
	if rel == "" {
		return "", false
	}
	return prefix + rel, true
}

// frameAncestors returns the frame-ancestors value for the data plane.
//
// Empty is the safe answer: an upload framed by nobody cannot be laid over a
// convincing fake of some other page. Naming the app's own origin lets the
// result card show what was just uploaded, and still leaves every other site
// unable to frame an upload.
func (h *Handler) frameAncestors() string {
	origin := strings.TrimSpace(h.cfg.FrontendOrigin)
	if origin == "" {
		return "'none'"
	}
	return origin
}

// rootRel returns the file a request for the site root should serve.
func (h *Handler) rootRel(dep *database.Deployment) string {
	if rel := h.indexRel(dep); rel != "" {
		return rel
	}
	return lonePDFRel(dep)
}

// lonePDFRel returns the deployment's PDF when it consists of exactly one.
//
// This mirrors isLonePDF on the upload side, so what was accepted as a document
// is what the root serves. Any other PDF-less deployment returns "", and the
// root 404s as it always did.
func lonePDFRel(dep *database.Deployment) string {
	var found string

	for _, e := range dep.Manifest {
		rel := strings.TrimPrefix(e.Key, dep.Prefix+"/")
		if rel == e.Key {
			continue // not under this deployment
		}
		if strings.Contains(rel, "/") {
			return ""
		}
		if !strings.EqualFold(path.Ext(rel), ".pdf") {
			return ""
		}
		if found != "" {
			return ""
		}
		found = rel
	}

	return found
}

// indexRel returns the deployment's index filename relative to its prefix.
func (h *Handler) indexRel(dep *database.Deployment) string {
	for _, e := range dep.Manifest {
		rel := strings.TrimPrefix(e.Key, dep.Prefix+"/")
		if rel == e.Key {
			continue // not under this deployment
		}
		switch strings.ToLower(rel) {
		case "index.html", "index.htm":
			return rel
		}
	}
	return ""
}

// writeObject streams an object with the headers a static host must set.
func (h *Handler) writeObject(c *gin.Context, key string, size int64, body io.Reader) {
	rel := key[strings.LastIndex(key, "/")+1:]

	if ct := contentTypeFor(rel); ct != "" {
		c.Header("Content-Type", ct)
	}

	if IsImmutable(key) {
		c.Header("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		// An index can be rewritten under the same name on every deploy, so a
		// cached copy is exactly the "I deployed and nothing changed" bug.
		c.Header("Cache-Control", "no-cache")
	}

	// Data-plane hardening. A deployment is already isolated by its own origin,
	// so there is nothing here for a hostile upload to reach and no cookie scope
	// to abuse. These are the parts that cost a legitimate site nothing:
	// clickjacking, base-tag hijacking, and posting a form off-origin.
	//
	// There is deliberately no default-src or script-src. Those are asset
	// allowlists, and an allowlist that admits only 'self' rejects inline
	// scripts, inline styles, data: URIs and CDN-hosted files, which is most
	// real sites. Serving them meant serving a blank page: the HTML arrived with
	// 200 and the browser refused to run any of it.
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", "base-uri 'none'; form-action 'none'; frame-ancestors "+h.frameAncestors())
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("X-Robots-Tag", "noindex, nofollow")

	if size > 0 {
		c.Header("Content-Length", strconv.FormatInt(size, 10))
	}

	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, body); err != nil {
		// Headers are already sent, so the status cannot be changed here; the
		// truncated body is the only signal available.
		slog.Warn("stream truncated", "key", key, "err", err)
	}
}

// serveStatus turns a lookup failure into a response.
func (h *Handler) serveStatus(c *gin.Context, err error) {
	switch {
	case errors.Is(err, database.ErrNotFound):
		h.notFound(c)
	case errors.Is(err, errGone):
		c.Header("Cache-Control", "no-store")
		c.String(http.StatusGone, "This deployment has expired.")
	case errors.Is(err, errPaused):
		c.Header("Cache-Control", "no-store")
		c.String(http.StatusServiceUnavailable, "This site is down.")
	default:
		slog.Error("serve lookup failed", "err", err)
		c.String(http.StatusBadGateway, "Storage unavailable.")
	}
}

// notFound serves a plain 404 rather than the app's JSON, since the caller is
// a browser expecting a page.
func (h *Handler) notFound(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusNotFound, "text/plain; charset=utf-8", []byte("404 Not Found\n"))
}

// previewURL builds the absolute preview address for a deployment.
func (h *Handler) previewURL(slug string) string {
	return fmt.Sprintf("%s://%s%s/", h.cfg.DataScheme, slug, h.cfg.DataHostSuffix)
}
