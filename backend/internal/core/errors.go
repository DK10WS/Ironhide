package core

import "errors"

// ErrQuota is returned when an upload would take the account past its
// allowance. The size of the request is the problem, not the request itself.
var ErrQuota = errors.New("storage quota exceeded")

// ErrGone is returned when a deployment is no longer available: retired,
// deleted, or past its claim window.
var ErrGone = errors.New("deployment is no longer available")

// ErrPaused is returned when the owner has taken the site down.
var ErrPaused = errors.New("site is down")
