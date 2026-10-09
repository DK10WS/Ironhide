// Command api serves static deployments out of Garage.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/dk10ws/ironhide/internal/api"
	"github.com/dk10ws/ironhide/internal/config"
	"github.com/dk10ws/ironhide/internal/database"
	"github.com/dk10ws/ironhide/internal/garage"
	"github.com/dk10ws/ironhide/internal/lifecycle"
	"github.com/dk10ws/ironhide/internal/reconciler"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	// Cancel on SIGTERM so in-flight requests finish and the pool closes
	// cleanly. Without this a rolling deploy drops connections.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	db, err := database.Open(ctx, cfg.Postgres)
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			slog.Warn("close postgres", "err", err)
		}
	}()

	if err := db.Migrate(); err != nil {
		return err
	}

	store, err := garage.NewStore(ctx, cfg)
	if err != nil {
		return err
	}

	// Background maintenance, started here rather than in the router so it is
	// tied to the process lifecycle and never runs during tests.
	//
	// The reconciler abandons uploads that never finished. Destruction is
	// opt-in and off by default: it is the only code that deletes objects, and
	// a fresh checkout should not start removing things unasked.
	if cfg.Reconcile.Interval > 0 {
		go reconciler.New(db).Run(ctx, cfg.Reconcile.Interval)
	}
	if cfg.Purge.Interval > 0 {
		go lifecycle.New(db, store).Run(ctx, cfg.Purge.Interval)
	}
	// Removing expired drops shares the purger's deletion path, so there is one
	// place that deletes objects rather than two.
	if cfg.Sweep.Interval > 0 {
		go lifecycle.NewSweeper(db, store).Run(ctx, cfg.Sweep.Interval)
	}

	slog.Info("starting",
		"addr", cfg.Addr,
		"endpoint", cfg.Endpoint,
		"bucket", cfg.Bucket,
		"postgres", cfg.Postgres.Host+":"+strconv.Itoa(cfg.Postgres.Port)+"/"+cfg.Postgres.Database,
	)

	router := api.NewRouter(cfg, store, db)
	return router.Run(cfg.Addr)
}
