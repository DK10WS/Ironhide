package core

import (
	"context"
	"io"
	"time"

	"github.com/dk10ws/ironhide/internal/database"
)

// GetObject reads one stored object. The data plane streams it back, so the
// caller owns closing the body.
func (c *Core) GetObject(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	return c.store.GetObject(ctx, key)
}

// Lookup resolves a slug to a deployment that may be served.
//
// The errors are the answer a data plane needs: database.ErrNotFound for an
// address that names nothing, ErrGone for one that was retired or ran out of
// claim window, and ErrPaused for one its owner took down.
func (c *Core) Lookup(ctx context.Context, slug string) (*database.Deployment, error) {
	dep, err := c.db.GetDeploymentByPrefix(ctx, "uploads/"+slug)
	if err != nil {
		return nil, err
	}

	switch {
	case dep.Status != database.StatusLive:
		return nil, ErrGone
	case dep.Expired(time.Now()):
		// Expired but not yet swept. Serving it would defeat the claim window,
		// so the sweeper's absence must not leak content.
		return nil, ErrGone
	case dep.SitePaused:
		// The owner took the site down. It is not expired and not missing, so
		// it gets its own answer rather than being folded into either.
		return nil, ErrPaused
	}

	return dep, nil
}
