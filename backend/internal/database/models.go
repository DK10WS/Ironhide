package database

import (
	"time"

	"gorm.io/gorm"
)

// Deployment states.
//
// A drop moves pending -> live on upload, and live -> expired when the claim
// window closes. Claiming does not change the state, only the owner, because
// a claimed drop is still live and still serves.
const (
	StatusPending = "pending"
	StatusLive    = "live"
	StatusExpired = "expired"
	StatusDeleted = "deleted"
)

type Site struct {
	ID   uint   `gorm:"primaryKey"`
	Slug string `gorm:"size:63;uniqueIndex;not null"`

	Domain string `gorm:"size:253;uniqueIndex;not null"`

	OwnerID string `gorm:"size:64;index"`

	Private bool `gorm:"not null;default:false"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

type Deployment struct {
	ID     uint `gorm:"primaryKey"`
	SiteID uint `gorm:"index;not null"`
	Site   Site `gorm:"foreignKey:SiteID;constraint:OnDelete:CASCADE"`

	Prefix string `gorm:"size:128;index;not null"`

	Status string `gorm:"size:16;index;not null;default:pending"`

	FileCount int   `gorm:"not null;default:0"`
	Bytes     int64 `gorm:"not null;default:0"`

	Manifest []byte `gorm:"type:jsonb"`

	ClaimTokenHash string `gorm:"size:64;index"`

	ExpiresAt time.Time  `gorm:"index"`
	ClaimedAt *time.Time `gorm:"index"`
	ClaimedBy string     `gorm:"size:64"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

func (d *Deployment) Expired(now time.Time) bool {
	return !d.ExpiresAt.IsZero() && d.ExpiresAt.Before(now)
}

func (d *Deployment) Claimed() bool {
	return d.ClaimedAt != nil
}

func (d *Deployment) PublicURLPath() string {
	return "/d/" + d.Prefix
}

func (d *Deployment) BeforeCreate(tx *gorm.DB) error {
	if d.Status == "" {
		d.Status = StatusPending
	}
	return nil
}
