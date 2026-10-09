package lifecycle

import (
	"context"
	"log/slog"
	"time"

	"github.com/dk10ws/ironhide/internal/database"
)

// Batch bounds one sweep pass.
const Batch = 100

// Sweeper removes deployments whose claim window has closed and that nobody
// claimed.
//
// It shares Purger.Purge with the purger rather than reimplementing the
// deletion, so both paths delete objects before the row and both leave a
// retryable record if the delete fails. The only difference is where the work
// list comes from: expired live drops here, failed rows in the Purger.
type Sweeper struct {
	purger *Purger
}

// NewSweeper returns a Sweeper that deletes through purger.
func NewSweeper(db *database.Database, store ObjectStore) *Sweeper {
	return &Sweeper{purger: New(db, store)}
}

// Once runs a single sweep pass.
//
// There is no grace here, and that is deliberate rather than an oversight: the
// expiry is the contract, and the serve path already answers 410 the moment
// the window closes, so keeping the bytes longer would only cost storage for a
// site nobody can reach. The safety comes from the query instead, which
// excludes anything claimed and anything still uploading.
func (s *Sweeper) Once(ctx context.Context) (Result, error) {
	var res Result

	expired, err := s.purger.db.ListExpiredForSweep(ctx, Batch)
	if err != nil {
		return res, err
	}
	if len(expired) == 0 {
		return res, nil
	}

	for _, dep := range expired {
		// Claim from the status the row is actually in, and refuse anything
		// that is neither live nor already purging.
		//
		// Listing already excludes those statuses, so this looks redundant.
		// It is not: treating every non-purging row as live meant the claim
		// query was the only thing standing between a pending upload and a
		// destructive UPDATE. A new status would have been silently swept.
		var from string
		switch dep.Status {
		case database.StatusLive, database.StatusPurging:
			from = dep.Status
		default:
			continue
		}

		claimed, won, err := s.purger.db.ClaimForPurge(ctx, dep.ID, from)
		if err != nil {
			slog.Error("could not claim an expired deployment for sweeping",
				"id", dep.ID, "prefix", dep.Prefix, "err", err)
			res.Failed++
			continue
		}
		if !won {
			// Another pass took it, or it was claimed in the meantime.
			continue
		}

		deleted, err := s.purger.Purge(ctx, claimed)
		if err != nil {
			slog.Error("sweep failed, objects remain",
				"id", claimed.ID, "prefix", claimed.Prefix, "err", err)
			res.Failed++
			continue
		}

		res.Purged++
		res.Objects += deleted
		slog.Info("swept an expired drop",
			"id", claimed.ID, "prefix", claimed.Prefix, "objects", deleted)
	}

	return res, nil
}

// Run sweeps on a ticker until the context is cancelled.
func (s *Sweeper) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("sweeper stopped")
			return
		case <-ticker.C:
			res, err := s.Once(ctx)
			if err != nil {
				slog.Error("sweep pass failed", "err", err)
				continue
			}
			if res.Purged > 0 || res.Failed > 0 {
				slog.Info("sweep pass complete",
					"swept", res.Purged, "objects", res.Objects, "failed", res.Failed)
			}
		}
	}
}
