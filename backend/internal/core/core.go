// Package core holds every operation Ironhide performs, independent of how a
// caller reaches it.
//
// The HTTP transport in package api and the MCP transport in package mcp are
// adapters over this package. Anything that decides what a valid write is
// belongs here, so the two transports cannot disagree about it.
package core

import (
	"context"
	"io"

	"github.com/dk10ws/ironhide/internal/config"
	"github.com/dk10ws/ironhide/internal/database"
	"github.com/dk10ws/ironhide/internal/upload"
)

// Store is the object storage this package needs: writes for uploads, reads
// for serving, and single-key deletes for a redeploy that drops a file.
type Store interface {
	upload.ObjectStore
	GetObject(ctx context.Context, key string) (io.ReadCloser, int64, error)
	DeleteKey(ctx context.Context, key string) error
}

// Core performs the operations. It holds no transport state.
type Core struct {
	store Store
	db    *database.Database
	cfg   config.Config
}

// New binds the dependencies an operation needs.
func New(store Store, db *database.Database, cfg config.Config) *Core {
	return &Core{store: store, db: db, cfg: cfg}
}

// Ping reports whether the metadata database is reachable.
func (c *Core) Ping(ctx context.Context) error {
	return c.db.Ping(ctx)
}
