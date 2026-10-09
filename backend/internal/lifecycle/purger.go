// Package lifecycle removes deployments that are no longer wanted.
//
// It is the destructive half of the expiry machinery and the only component
// that deletes objects. Keeping it in its own package makes that boundary
// obvious: everything here can destroy a site, nothing else can.
//
// The removal is deliberately two-staged.
//
//	pending ──(reconciler, 1h grace)──► failed ──(purger, 24h grace)──► gone
//
// A row only loses its bytes a long time after it stopped being useful, so a
// false positive in the first stage has a full day to be noticed before
// anything is unrecoverable. Deleting in one step would mean a bug in the
// grace comparison could destroy live sites on its first run.
package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/dk10ws/ironhide/internal/database"
)

// ObjectStore is the storage surface the purger needs.
type ObjectStore interface {
	DeletePrefix(ctx context.Context, prefix string) (int, error)
}

// Grace is how long a failed deployment must stay failed before its objects
// are deleted.
//
// It is long on purpose. The reconciler that moves rows into failed can be
// wrong; this delay is the buffer between "we decided to stop trusting this"
// and "we destroyed it".
const Grace = 24 * time.Hour

// PurgeBatch bounds one purge pass. It is smaller than the sweep batch
// because a purge is the more dangerous of the two: it destroys rows nothing
// is waiting on, so a smaller slice means a mistake is smaller.
const PurgeBatch = 50

// Purger removes stale failed deployments.
type Purger struct {
	db    *database.Database
	store ObjectStore
}

// New returns a Purger.
func New(db *database.Database, store ObjectStore) *Purger {
	return &Purger{db: db, store: store}
}

// Result counts what a pass did.
type Result struct {
	// Purged is how many deployments had their objects and row removed.
	Purged int

	// Objects is how many objects were deleted in total.
	Objects int

	// Failed is how many deployments could not be purged, which always means
	// the objects are still there.
	Failed int
}

// Once runs a single purge pass.
func (p *Purger) Once(ctx context.Context) (Result, error) {
	var res Result

	candidates, err := p.db.ListStaleFailed(ctx, Grace, PurgeBatch)
	if err != nil {
		return res, err
	}
	if len(candidates) == 0 {
		return res, nil
	}

	for _, dep := range candidates {
		// Claim the row before deleting anything. Two purgers racing would
		// otherwise both list the same objects and delete them twice, which is
		// wasteful but also means neither knows which one owns the delete.
		claimed, won, err := p.db.ClaimForPurge(ctx, dep.ID, database.StatusFailed)
		if err != nil {
			slog.Error("could not claim a deployment for purging",
				"id", dep.ID, "prefix", dep.Prefix, "err", err)
			res.Failed++
			continue
		}
		if !won {
			// Another purger took it.
			continue
		}

		deleted, err := p.Purge(ctx, claimed)
		if err != nil {
			slog.Error("purge failed, objects remain",
				"id", claimed.ID, "prefix", claimed.Prefix, "err", err)
			res.Failed++
			continue
		}

		res.Purged++
		res.Objects += deleted
		slog.Info("purged", "id", claimed.ID, "prefix", claimed.Prefix, "objects", deleted)
	}

	return res, nil
}

// Purge removes a deployment's objects and then its row.
//
// Objects go first. The reverse order would leave a row pointing at nothing if
// the process died in between, which is a live-looking deployment whose every
// asset 404s. Objects with no row are merely invisible, and the row here is
// still findable.
func (p *Purger) Purge(ctx context.Context, dep *database.Deployment) (int, error) {
	// The stored prefix has no trailing slash; object keys do.
	deleted, err := p.store.DeletePrefix(ctx, dep.Prefix+"/")
	if err != nil {
		return deleted, fmt.Errorf("delete objects under %s: %w", dep.Prefix, err)
	}

	if err := p.db.DeleteDeployment(ctx, dep.ID); err != nil {
		return deleted, fmt.Errorf("delete row %d: %w", dep.ID, err)
	}
	return deleted, nil
}

// Run purges on a ticker until the context is cancelled.
//
// A failing pass is logged and retried rather than fatal: the objects are
// still there, and the next pass will try again.
func (p *Purger) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("purger stopped")
			return
		case <-ticker.C:
			res, err := p.Once(ctx)
			if err != nil {
				slog.Error("purge pass failed", "err", err)
				continue
			}
			if res.Purged > 0 || res.Failed > 0 {
				slog.Info("purge pass complete",
					"purged", res.Purged, "objects", res.Objects, "failed", res.Failed)
			}
		}
	}
}
