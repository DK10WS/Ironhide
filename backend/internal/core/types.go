package core

import (
	"time"

	"github.com/dk10ws/ironhide/internal/upload"
)

// Target says who an upload is for.
//
// The zero value is an anonymous drop: temporary, kept alive by a claim link.
// Naming an owner makes it a permanent site with no claim window.
type Target struct {
	// OwnerID is Google's stable subject id. Empty means anonymous.
	OwnerID string

	// SiteID is the site being deployed to. Zero means this upload creates a
	// new site rather than replacing one.
	SiteID uint

	// Prefix reuses an existing object prefix, which is what keeps a redeploy
	// on the same address. Empty means mint a fresh one.
	Prefix string
}

// Owned reports whether this upload belongs to an account.
func (t Target) Owned() bool { return t.OwnerID != "" }

// Payload is the content to deploy.
//
// Exactly one of Zip and Files is used. Zip is an archive body; Files is a
// set of files posted individually. Flatten applies only to an archive.
type Payload struct {
	Zip     []byte
	Files   []upload.IncomingFile
	Flatten bool
}

// Result describes a finished upload.
type Result struct {
	ID         uint
	Slug       string
	PreviewURL string

	// ClaimURL is empty for an owned upload, which needs no redemption.
	ClaimURL string

	// ExpiresAt is nil when the deployment is permanent.
	ExpiresAt *time.Time

	Bytes int64
	Files []upload.File

	Owned bool
}

// Site is one row of an account's dashboard.
type Site struct {
	ID   uint
	Slug string
	URL  string

	Bytes      int64
	Files      int
	DeployedAt *time.Time
	Live       bool
	Paused     bool
}

// Usage reports how much of an account's allowance is spent.
type Usage struct {
	Used  int64
	Limit int64
}

// Claim is the outcome of redeeming a claim token.
type Claim struct {
	ID        uint
	URL       string
	ClaimedAt time.Time
}
