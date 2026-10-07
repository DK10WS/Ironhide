// Package api is the HTTP transport.
//
// It serves two planes on one listener:
//
//	<control host>            control plane: /upload, /claim, /health, /mcp
//	<slug><data suffix>/      data plane: the uploaded sites
//
// The split exists because the data plane serves content anyone can upload.
// Putting it on the control plane's origin would give that content the same
// origin as anything authenticated, so uploaded script could reach a session.
// Separate hosts mean separate origins and no cookie scope to abuse.
package api

import (
	"context"
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

	control := r.Group("/", onlyControlHost(cfg.ControlHost))
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

// claim redeems a single-use claim token, attaching a drop to a site.
func (h *Handler) claim(c *gin.Context) {
	token := c.Param("token")
	if token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing token"})
		return
	}

	// A claim without a site to attach to has nowhere to go yet, so the
	// endpoint reports what it needs rather than pretending to succeed.
	c.JSON(http.StatusNotImplemented, gin.H{
		"error": "claiming needs a target site, which is not wired up yet",
	})

	// The token is deliberately absent from this log line. It is the only
	// credential for an unclaimed drop, and stderr is shipped to a collector;
	// even a prefix would cut a 256-bit secret's entropy for no benefit.
	slog.Info("claim attempted")
}

func requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		slog.Info("request",
			"method", c.Request.Method,
			"host", c.Request.Host,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"dur_ms", time.Since(start).Milliseconds(),
		)
	}
}
