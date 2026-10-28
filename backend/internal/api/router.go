// Package api is the HTTP transport.
//
// It serves two planes on one listener:
//
//	<control host>/api/       control plane: /api/upload, /api/claim, /api/health
//	<slug><data suffix>/      data plane: the uploaded sites
//
// The split exists because the data plane serves content anyone can upload.
// Putting it on the control plane's origin would give that content the same
// origin as anything authenticated, so uploaded script could reach a session.
// Separate hosts mean separate origins and no cookie scope to abuse.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/dk10ws/ironhide/internal/config"
	"github.com/dk10ws/ironhide/internal/core"
	"github.com/dk10ws/ironhide/internal/database"
)

// Handler holds the dependencies shared by every route.
//
// The database is here for sign-in only: sessions are a credential concern and
// live in the transport that issues the cookie. Everything that acts on
// deployments goes through core.
type Handler struct {
	core *core.Core
	db   *database.Database
	cfg  config.Config
}

func NewRouter(cfg config.Config, store core.Store, db *database.Database) *gin.Engine {
	h := &Handler{core: core.New(store, db, cfg), db: db, cfg: cfg}

	gin.SetMode(gin.ReleaseMode)

	r := gin.New()
	r.Use(gin.Recovery(), requestLogger())

	control := r.Group("/api", onlyControlHost(cfg.ControlHost))
	{
		control.GET("/health", h.health)
		control.GET("/health/ready", h.ready)

		uploads := control.Group("/upload")
		{
			// optionalAccount makes a signed-in upload an owned site without
			// requiring one: with no session it stays a temporary drop with a
			// claim link, which is the behaviour that existed before accounts
			// and is still the only option for an anonymous caller.
			uploads.POST("/zip", h.optionalAccount, h.ownedUploadZip)
			uploads.POST("/file", h.optionalAccount, h.uploadFile)
			uploads.POST("/files", h.optionalAccount, h.ownedUploadFiles)
		}

		control.POST("/claim/:token", h.claim)

		// Not registered when Google credentials are absent, so an
		// unauthenticated deployment offers no half-working sign-in and no
		// endpoints that can only ever answer 401.
		if h.authEnabled() {
			h.registerAuth(control)
			h.registerSites(control)
		}
	}

	r.NoRoute(h.serve)

	return r
}

// onlyControlHost rejects requests that did not arrive on the control host.
func onlyControlHost(want string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !sameHost(c.Request.Host, want) {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	}
}

// sameHost compares two host[:port] values, ignoring case and an absent port.
func sameHost(got, want string) bool {
	g, _, _ := strings.Cut(got, ":")
	w, wport, _ := strings.Cut(want, ":")

	if !strings.EqualFold(g, w) {
		return false
	}
	// If the configured host pins a port, the request must match it; that
	// matters when the control plane and data plane share a listener but not a
	// port in front of a load balancer.
	if wport != "" {
		_, gport, _ := strings.Cut(got, ":")
		return gport == wport
	}
	return true
}

func (h *Handler) health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ready"})
}

// ready is the readiness probe. Traffic may only be routed here when the
// database is reachable.
func (h *Handler) ready(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()

	if err := h.core.Ping(ctx); err != nil {
		slog.Warn("readiness failed", "err", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ready"})
}

// claim redeems a claim token, making an anonymous drop permanent.
//
// The token in the URL is the entire credential. There is no session, no API
// key and no second factor, which is a deliberate simplification: the token is
// 256 bits of CSPRNG output, single-use, and only its hash is stored. The cost
// is that a leaked claim_url can be redeemed by whoever holds it, and that
// there is no way to revoke one early.
//
// Nothing about the token is logged. It is the only credential on this path,
// and stderr is shipped onward to a collector.
func (h *Handler) claim(c *gin.Context) {
	claimed, err := h.core.Claim(c.Request.Context(), c.Param("token"))
	if err != nil {
		switch {
		case errors.Is(err, database.ErrAlreadyClaimed):
			// Ambiguous on purpose: a spent token and one that never existed
			// are indistinguishable, so a caller cannot use the response to
			// discover which tokens are outstanding.
			c.JSON(http.StatusGone, gin.H{
				"error": "this claim link has already been used or has expired",
			})
		case errors.Is(err, database.ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "no such claim link"})
		default:
			h.fail(c, err)
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"id":         claimed.ID,
		"site_url":   claimed.URL,
		"claimed_at": claimed.ClaimedAt.UTC().Format(time.RFC3339),
	})
}

// requestLogger records one line per request.
func requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		slog.Info("request",
			"method", c.Request.Method,
			"host", c.Request.Host,
			"path", redactPath(c.Request.URL.Path),
			"status", c.Writer.Status(),
			"dur_ms", time.Since(start).Milliseconds(),
		)
	}
}

// redactPath strips credentials out of a path before it is logged.
//
// The claim token arrives as a path segment, so logging the path verbatim
// writes the only credential on that route into stderr, which is shipped
// onward to a collector. Gin's own recovery log can include the path too, so
// this is not the only place the value could escape.
func redactPath(p string) string {
	const prefix = "/api/claim/"
	if !strings.HasPrefix(p, prefix) {
		return p
	}
	token := strings.TrimPrefix(p, prefix)
	if token == "" {
		return p
	}
	// Enough to correlate two attempts at the same link, not enough to use it.
	const keep = 8
	if len(token) <= keep {
		return prefix + "***"
	}
	return prefix + "***" + token[len(token)-keep:]
}
