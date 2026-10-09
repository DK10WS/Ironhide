// Package reconciler abandons uploads that never completed.
//
// An upload writes its deployment row before its objects, so a crash between
// the two leaves a pending row rather than orphaned bytes. That direction was
// chosen deliberately: this package can find stuck rows with one indexed
// query, whereas orphaned objects would mean listing every key in the bucket.
//
// It deliberately does not delete objects. A pending row's objects are
// harmless once the row is failed, and reaching into the bucket from here
// would put the one component that can delete live data in the same place as
// the component that creates it.
package reconciler

import (
	"context"
	"log/slog"
	"time"

	"github.com/dk10ws/ironhide/internal/database"
)

// Grace is how long a pending deployment may sit before it is abandoned.
//
// It must comfortably exceed the slowest plausible upload, because a row that
// is abandoned while its objects are still being written produces a site that
// never serves. One hour is far beyond any upload this accepts, given the
// request body cap, and matches the default claim window.
const Grace = time.Hour

// Batch bounds one reconciliation pass.
const Batch = 100

// Result counts what a pass did.
type Result struct {
	// Abandoned is how many pending rows were moved to failed.
	Abandoned int
}

// Reconciler abandons stuck uploads.
type Reconciler struct {
	db *database.Database
}

// New returns a Reconciler.
func New(db *database.Database) *Reconciler {
	return &Reconciler{db: db}
}

// Once runs a single reconciliation pass.
//
// It is separate from Run so it can be tested without a timer, and so it can
// be driven by a cron schedule rather than an in-process ticker.
func (r *Reconciler) Once(ctx context.Context) (Result, error) {
	stale, err := r.db.MarkStalePendingFailed(ctx, Grace, Batch)
	if err != nil {
		return Result{}, err
	}

	if len(stale) == 0 {
		return Result{}, nil
	}

	for _, d := range stale {
		// The prefix is logged because it is the only way to find and remove
		// whatever objects a half-finished upload managed to write.
		slog.Warn("abandoned a stuck upload",
			"id", d.ID,
			"prefix", d.Prefix,
			"age", time.Since(d.CreatedAt).Round(time.Minute).String(),
		)
	}

	return Result{Abandoned: len(stale)}, nil
}

// Run reconciles on a ticker until the context is cancelled.
//
// It logs the first failure and keeps going: a transient database blip should
// not stop the reconciler permanently, and a permanently broken one shows up
// as a repeated error on every tick.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("reconciler stopped")
			return
		case <-ticker.C:
			res, err := r.Once(ctx)
			if err != nil {
				slog.Error("reconcile pass failed", "err", err)
				continue
			}
			if res.Abandoned > 0 {
				slog.Info("reconcile pass complete", "abandoned", res.Abandoned)
			}
		}
	}
}
