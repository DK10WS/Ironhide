package config

import (
	"errors"
	"os"
)

type Config struct {
	// Addr is the listen address for the HTTP server, e.g. ":8080"
	Addr string

	// Garage S3 settings.
	Endpoint  string
	Region    string
	AccessKey string
	SecretKey string
	Bucket    string
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
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
	}

	if c.AccessKey == "" || c.SecretKey == "" {
		return c, errors.New("GARAGE_ACCESS_KEY and GARAGE_SECRET_KEY must be set")
	}
	return c, nil
}
