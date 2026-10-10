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

	// FrontendOrigin is the one origin allowed to embed a preview in an
	// iframe, sent as the data plane's frame-ancestors.
	//
	// Empty means 'none', which is the safe default and what an unauthenticated
	// deployment wants: an upload can then not be framed by anyone, so it cannot
	// be used to overlay a convincing fake of some other page. Naming a single
	// origin keeps that property for every site except the one serving this app.
	FrontendOrigin string

	// Google holds the OAuth client used to sign people in.
	Google Google

	// Session configures the cookie that carries a signed-in session.
	Session Session

	// Quota caps how much a single account may store.
	Quota Quota

	// DataHostSuffix is the suffix previews are served under. A deployment
	// with slug abc123 is served at abc123<DataHostSuffix>, which gives every
	// upload its own origin so hostile content cannot reach another upload's
	// storage, and gives the data plane no cookie scope to abuse.
	DataHostSuffix string

	// DataScheme is the scheme used in built preview URLs.
	DataScheme string

	// Upload settings.
	Upload Upload

	// Reconcile controls the stuck-upload reconciler.
	Reconcile Reconcile

	// Purge controls destruction of failed deployments.
	Purge Purge

	// Sweep controls removal of expired, unclaimed drops.
	Sweep Sweep
}

// Sweep controls how often expired drops lose their objects.
//
// Interval zero disables it. Unlike the purger this needs no default grace of
// its own: the expiry is the contract, and the serve path already answers 410
// once the window closes.
type Sweep struct {
	Interval time.Duration
}

// Purge controls how often failed deployments lose their objects.
//
// Interval zero disables it. It is off by default because it is the only
// component that deletes data, and a fresh checkout should not start removing
// things on its own.
type Purge struct {
	Interval time.Duration
}

// Reconcile controls how often stuck uploads are abandoned.
//
// Interval zero disables it, which is what the test suite relies on so no
// background pass can delete a row a test is asserting on.
type Reconcile struct {
	Interval time.Duration
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

// Google holds the OAuth client credentials for sign-in.
//
// Empty ClientID disables the sign-in routes, which is what an unauthenticated
// deployment wants: they are not registered rather than registered and broken.
type Google struct {
	ClientID     string
	ClientSecret string

	// RedirectURI must match the one registered with Google exactly, callback
	// path included. Google rejects the exchange otherwise.
	RedirectURI string

	// The three Google endpoints the flow talks to. They are fields rather than
	// constants so a test can point them at a local stand-in and exercise the
	// half of sign-in that a live Google account is otherwise the only way to
	// reach: exchanging the code, reading the profile, and issuing the cookie.
	// Empty means the real Google, and Load fills the defaults in.
	AuthURL     string
	TokenURL    string
	UserInfoURL string
}

// AuthEnabled reports whether sign-in can work at all.
func (g Google) AuthEnabled() bool {
	return g.ClientID != "" && g.ClientSecret != "" && g.RedirectURI != ""
}

// Google's own endpoints, used whenever the matching field is blank.
const (
	defaultAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	defaultTokenURL    = "https://oauth2.googleapis.com/token"
	defaultUserInfoURL = "https://www.googleapis.com/oauth2/v3/userinfo"
)

// Endpoints returns the three URLs the flow talks to, substituting Google's
// real ones for any left blank.
//
// Load fills them in, but a config built by hand — which is how tests make one —
// would otherwise leave them empty and send requests to nothing.
func (g Google) Endpoints() (authURL, tokenURL, userInfoURL string) {
	authURL, tokenURL, userInfoURL = g.AuthURL, g.TokenURL, g.UserInfoURL
	if authURL == "" {
		authURL = defaultAuthURL
	}
	if tokenURL == "" {
		tokenURL = defaultTokenURL
	}
	if userInfoURL == "" {
		userInfoURL = defaultUserInfoURL
	}
	return authURL, tokenURL, userInfoURL
}

// Session configures the cookie carrying a signed-in session.
//
// The cookie holds a random token whose hash is the only thing stored, so a
// database dump does not hand out live sessions. Lifetime is short enough that
// a stolen cookie expires on its own.
type Session struct {
	// Lifetime is how long a session stays valid.
	Lifetime time.Duration

	// Secure marks the cookie Secure. It should be false only for local http,
	// where a Secure cookie is silently dropped and sign-in appears broken.
	Secure bool
}

// Quota caps what one account may store.
type Quota struct {
	// BytesPerUser is the total of the current live deployments owned by one
	// account. A redeploy replaces the files it supersedes, so an old
	// deployment's size stops counting once its objects are gone.
	BytesPerUser int64
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
		FrontendOrigin: env("FRONTEND_ORIGIN", ""),

		Google: Google{
			ClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
			ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
			RedirectURI:  env("GOOGLE_REDIRECT_URI", "http://localhost:7210/api/auth/google/callback"),
			AuthURL:      env("GOOGLE_AUTH_URL", "https://accounts.google.com/o/oauth2/v2/auth"),
			TokenURL:     env("GOOGLE_TOKEN_URL", "https://oauth2.googleapis.com/token"),
			UserInfoURL:  env("GOOGLE_USERINFO_URL", "https://www.googleapis.com/oauth2/v3/userinfo"),
		},

		Session: Session{
			Lifetime: time.Duration(envInt64("SESSION_LIFETIME_SECONDS", 30*24*3600)) * time.Second,
			Secure:   env("SESSION_COOKIE_SECURE", "false") == "true",
		},

		Quota: Quota{
			BytesPerUser: envInt64("QUOTA_BYTES_PER_USER", 1<<30),
		},

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

		Purge: Purge{
			Interval: time.Duration(envInt64("PURGE_INTERVAL_SECONDS", 0)) * time.Second,
		},

		Sweep: Sweep{
			Interval: time.Duration(envInt64("SWEEP_INTERVAL_SECONDS", 0)) * time.Second,
		},

		Reconcile: Reconcile{
			// Zero disables the reconciler, so a bare `go run` does not age
			// out rows that a developer is looking at.
			Interval: time.Duration(envInt64("RECONCILE_INTERVAL_SECONDS", 0)) * time.Second,
		},

		Upload: Upload{
			// MaxBytes is the request ceiling and sits well above the sum the
			// extract caps allow, so the extract caps are what actually trip
			// first and report a useful reason. It is deliberately loose: a
			// request that reaches MaxBytes got a bare transport-level refusal
			// instead of a sentence explaining which limit it broke.
			MaxBytes: envInt64("MAX_UPLOAD_BYTES", 256<<20),

			// The three below are the real ceiling, all the same 50 MiB.
			// A single file, an archive, and the total of everything extracted
			// are bounded separately so each refusal names the thing that was
			// actually too big rather than a generic limit.
			ArchiveBytes: envInt64("MAX_ARCHIVE_BYTES", 50<<20),
			FileBytes:    envInt64("MAX_FILE_BYTES", 50<<20),
			TotalBytes:   envInt64("MAX_TOTAL_BYTES", 50<<20),

			MaxEntries: envInt("MAX_ENTRIES", 4096),
			TTL:        time.Duration(envInt64("UPLOAD_TTL_SECONDS", 3600)) * time.Second,
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
