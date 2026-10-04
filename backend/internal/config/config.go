package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

// Config holds every setting the api needs.
type Config struct {
	// Addr is the listen address for the HTTP server, e.g. ":2345"
	Addr string

	// Garage S3 settings.
	Endpoint  string
	Region    string
	AccessKey string
	SecretKey string
	Bucket    string

	// Postgres holds deployment metadata.
	Postgres Postgres
}

// Postgres identifies the metadata database.
type Postgres struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string
	SSLMode  string
}

// DSN renders a libpq connection string.
//
// The result contains the password, so it must never be logged. Errors from
// the driver are sanitised before they reach a log line for this reason.
func (p Postgres) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s TimeZone=UTC",
		p.Host, p.Port, p.User, p.Password, p.Database, p.SSLMode,
	)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

func Load() (Config, error) {
	c := Config{
		Addr:      env("ADDR", ":2345"),
		Endpoint:  env("GARAGE_ENDPOINT", "http://localhost:3900"),
		Region:    env("GARAGE_REGION", "garage"),
		AccessKey: os.Getenv("GARAGE_ACCESS_KEY"),
		SecretKey: os.Getenv("GARAGE_SECRET_KEY"),
		Bucket:    env("GARAGE_BUCKET", "deployment"),

		Postgres: Postgres{
			// The default host is the compose service name, which only
			// resolves inside the network. Override it when running the
			// binary on the host.
			Host:     env("POSTGRES_HOST", "postgres"),
			Port:     envInt("POSTGRES_PORT", 5432),
			User:     env("POSTGRES_USER", "ironhide"),
			Password: os.Getenv("POSTGRES_PASSWORD"),
			Database: env("POSTGRES_DB", "ironhide"),
			// Local dev has no TLS; require it anywhere real.
			SSLMode: env("POSTGRES_SSLMODE", "disable"),
		},
	}

	if c.AccessKey == "" || c.SecretKey == "" {
		return c, errors.New("GARAGE_ACCESS_KEY and GARAGE_SECRET_KEY must be set")
	}
	if c.Postgres.Password == "" {
		return c, errors.New("POSTGRES_PASSWORD must be set")
	}
	return c, nil
}
