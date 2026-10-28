// Command mcp serves Ironhide's operations over the Model Context Protocol.
//
// It is a second front end on the same core the HTTP API uses, so it talks to
// Postgres and Garage directly rather than through the API. That makes it an
// in-cluster or port-forwarded process, not something a remote client can run.
//
// Stdout carries the protocol, so every log line goes to stderr.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dk10ws/ironhide/internal/config"
	"github.com/dk10ws/ironhide/internal/core"
	"github.com/dk10ws/ironhide/internal/database"
	"github.com/dk10ws/ironhide/internal/garage"
	"github.com/dk10ws/ironhide/internal/mcp"
)

const version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

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

	// The API owns the schema. Migrating from a second binary would race it on
	// every start, so this only checks the database is there.
	if err := db.Ping(ctx); err != nil {
		return err
	}

	store, err := garage.NewStore(ctx, cfg)
	if err != nil {
		return err
	}

	// No reconciler, purger or sweeper: maintenance is bound to the API process
	// so exactly one binary ever runs it.
	server := sdk.NewServer(&sdk.Implementation{
		Name:        "ironhide",
		Title:       "Ironhide",
		Description: "Deploy a static site and get its public URL.",
		Version:     version,
	}, nil)

	mcp.Register(server, core.New(store, db, cfg))

	slog.Info("starting",
		"transport", "stdio",
		"postgres", cfg.Postgres.Host+":"+strconv.Itoa(cfg.Postgres.Port)+"/"+cfg.Postgres.Database,
	)

	return server.Run(ctx, &sdk.StdioTransport{})
}
