// Command api serves static deployments out of Garage.
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/dk10ws/ironhide/internal/api"
	"github.com/dk10ws/ironhide/internal/config"
	"github.com/dk10ws/ironhide/internal/garage"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(context.Background()); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	client, err := garage.New(ctx, cfg)
	if err != nil {
		return err
	}

	slog.Info("starting",
		"addr", cfg.Addr,
		"endpoint", cfg.Endpoint,
		"bucket", cfg.Bucket,
	)

	return api.NewRouter(client, cfg.Bucket).Run(cfg.Addr)
}
