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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/dk10ws/ironhide/internal/config"
	"github.com/dk10ws/ironhide/internal/database"
	"github.com/dk10ws/ironhide/internal/upload"
)

// objectStore is the storage surface this package needs: writes for the upload
// routes, reads for the data plane.
//
// Declared here rather than importing package garage, so the transport depends
// on behaviour and not on the S3 client behind it.
type objectStore interface {
	upload.ObjectStore
	GetObject(ctx context.Context, key string) (io.ReadCloser, int64, error)
}

// Handler holds the dependencies shared by every route.
type Handler struct {
	store objectStore
	db    *database.Database
	cfg   config.Config
}

func NewRouter(cfg config.Config, store objectStore, db *database.Database) *gin.Engine {
	h := &Handler{store: store, db: db, cfg: cfg}

	gin.SetMode(gin.ReleaseMode)

	r := gin.New()
	r.Use(gin.Recovery(), requestLogger())

	control := r.Group("/api", onlyControlHost(cfg.ControlHost))
	{
		control.GET("/health", h.health)
		control.GET("/health/ready", h.ready)

		uploads := control.Group("/upload")
		{
			uploads.POST("/zip", h.uploadZip)
			uploads.POST("/file", h.uploadFile)
			uploads.POST("/files", h.uploadFiles)
		}

		control.POST("/claim/:token", h.claim)
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

	if err := h.db.Ping(ctx); err != nil {
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
	token := c.Param("token")
	if token == "" {
		h.fail(c, fmt.Errorf("%w: missing claim token", database.ErrNotFound))
		return
	}

	dep, err := h.db.ClaimPermanently(c.Request.Context(), token, claimedByToken)
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

	slog.Info("deployment claimed", "id", dep.ID, "prefix", dep.Prefix)

	c.JSON(http.StatusOK, gin.H{
		"id":         dep.ID,
		"site_url":   h.previewURL(dep.Slug()),
		"claimed_at": dep.ClaimedAt.UTC().Format(time.RFC3339),
	})
}

// claimedByToken records who claimed a deployment when the token is the only
// credential available. It is a fixed string rather than the token itself, so
// no secret reaches the database or the logs.
const claimedByToken = "claim-token"

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
