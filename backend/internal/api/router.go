package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	s3     *s3.Client
	bucket string
}

func NewRouter(s3Client *s3.Client, bucket string) *gin.Engine {
	h := &Handler{
		s3:     s3Client,
		bucket: bucket,
	}

	r := gin.New()
	r.Use(gin.Recovery(), requestLogger())

	r.GET("/health", h.health)

	return r
}

func (h *Handler) health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		slog.Info("request",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"dur_ms", time.Since(start).Milliseconds(),
		)
	}
}
