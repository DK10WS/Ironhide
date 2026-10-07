package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dk10ws/ironhide/internal/upload"
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

	// PublicURL is the externally visible origin of the control plane, used to
	// build absolute claim links. A claim token in a URL that only resolves
	// internally is useless, and this token is what grants ownership.
	PublicURL string

	// ControlHost is the host[:port] that serves the control plane. Requests
	// arriving anywhere else are not given control routes.
	ControlHost string

	// DataHostSuffix is the suffix previews are served under. A deployment
	// with slug abc123 is served at abc123<DataHostSuffix>, which gives every
	// upload its own origin so hostile content cannot reach another upload's
	// storage, and gives the data plane no cookie scope to abuse.
	DataHostSuffix string

	// DataScheme is the scheme used in built preview URLs.
	DataScheme string

	// Upload settings.
	Upload Upload
}

// Upload caps what a single request may cost.
//
// These are server-side, not per-request: a client must not be able to choose
// its own limits. MaxBytes is the hard ceiling on the HTTP body, and the rest
// bound what may be extracted from within it.
type Upload struct {
	// MaxBytes caps the whole request. A body over this is refused while
	// streaming, before anything is buffered.
	MaxBytes int64

	// ArchiveBytes caps a compressed zip.
	ArchiveBytes int64

	// FileBytes caps one file, whether it came from a zip or a part.
	FileBytes int64

	// TotalBytes caps everything extracted from one request combined.
	TotalBytes int64

	// MaxEntries caps how many files one request may carry.
	MaxEntries int

	// TTL is how long an unclaimed upload stays live.
	TTL time.Duration
}

// Limits converts to the form the upload package takes.
func (u Upload) Limits() upload.Limits {
	return upload.Limits{
		MaxArchiveBytes: u.ArchiveBytes,
		MaxFileBytes:    u.FileBytes,
		MaxTotalBytes:   u.TotalBytes,
		MaxEntries:      u.MaxEntries,
	}
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

func envInt64(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
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
		Addr:           env("ADDR", ":2345"),
		Endpoint:       env("GARAGE_ENDPOINT", "http://localhost:3900"),
		Region:         env("GARAGE_REGION", "garage"),
		AccessKey:      os.Getenv("GARAGE_ACCESS_KEY"),
		SecretKey:      os.Getenv("GARAGE_SECRET_KEY"),
		Bucket:         env("GARAGE_BUCKET", "deployment"),
		PublicURL:      strings.TrimSuffix(env("PUBLIC_URL", "http://localhost:2345"), "/"),
		ControlHost:    env("CONTROL_HOST", "localhost:2345"),
		DataHostSuffix: env("DATA_HOST_SUFFIX", ".preview.localhost"),
		DataScheme:     env("DATA_SCHEME", "http"),

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

		Upload: Upload{
			// MaxBytes is the request ceiling and sits above the sum the
			// extract caps allow, so the extract caps are what actually trip
			// first and report a useful reason.
			MaxBytes:     envInt64("MAX_UPLOAD_BYTES", 256<<20),
			ArchiveBytes: envInt64("MAX_ARCHIVE_BYTES", 128<<20),
			FileBytes:    envInt64("MAX_FILE_BYTES", 32<<20),
			TotalBytes:   envInt64("MAX_TOTAL_BYTES", 256<<20),
			MaxEntries:   envInt("MAX_ENTRIES", 4096),
			TTL:          time.Duration(envInt64("UPLOAD_TTL_SECONDS", 3600)) * time.Second,
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
