package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/dk10ws/ironhide/internal/config"
	"github.com/dk10ws/ironhide/internal/database"
)

// Cookie names. The session cookie is scoped to /api so it is never attached to
// a static asset request, and never travels to a preview host.
const (
	sessionCookie = "ironhide_session"
	stateCookie   = "ironhide_oauth_state"
)

// accountKey is where the signed-in account is stashed for the request.
const accountKey = "account"

// account is the caller behind a request.
type account struct {
	User        *database.User
	SessionHash string
}

// authEnabled reports whether the deployment has Google credentials. Without
// them the sign-in routes are not registered, so an unauthenticated
// installation exposes no half-working endpoints.
func (h *Handler) authEnabled() bool { return h.cfg.Google.AuthEnabled() }

// registerAuth wires the sign-in routes.
func (h *Handler) registerAuth(r *gin.RouterGroup) {
	r.GET("/auth/google", h.googleSignIn)
	r.GET("/auth/google/callback", h.googleCallback)
	r.POST("/auth/logout", h.logout)
	r.GET("/me", h.requireAccount, h.me)
}

// googleSignIn sends the browser to Google's consent screen.
//
// The state is a random value kept in a cookie and compared on the way back.
// Without it an attacker could feed a victim a callback carrying the attacker's
// code and silently sign them into the wrong account.
func (h *Handler) googleSignIn(c *gin.Context) {
	state, err := randomValue()
	if err != nil {
		h.fail(c, err)
		return
	}

	http.SetCookie(c.Writer, &http.Cookie{
		Name:     stateCookie,
		Value:    state,
		Path:     "/api",
		MaxAge:   int(600),
		HttpOnly: true,
		Secure:   h.cfg.Session.Secure,
		SameSite: http.SameSiteLaxMode,
	})

	query := url.Values{
		"client_id":     {h.cfg.Google.ClientID},
		"redirect_uri":  {h.cfg.Google.RedirectURI},
		"response_type": {"code"},
		"scope":         {"openid email profile"},
		"state":         {state},
		// Forces the consent screen every time, so a new deployment on this
		// domain cannot inherit a grant made for another one.
		"prompt": {"select_account"},
	}

	c.Redirect(http.StatusFound, "https://accounts.google.com/o/oauth2/v2/auth?"+query.Encode())
}

// googleCallback completes the exchange and starts a session.
//
// The code is exchanged server to server with the client secret, then the
// account is read from Google's userinfo endpoint over TLS. Reading the profile
// from Google directly rather than decoding the id_token locally avoids a JWT
// dependency without weakening anything: the exchange is already authenticated
// by the secret.
func (h *Handler) googleCallback(c *gin.Context) {
	cookie, err := c.Request.Cookie(stateCookie)
	if err != nil || cookie.Value == "" {
		h.authFail(c, "The sign-in attempt expired. Try again.")
		return
	}

	// Cleared whatever happens next, so a stale state cannot be replayed.
	clearCookie(c, h.cfg, stateCookie)

	if got := c.Query("state"); got == "" || got != cookie.Value {
		h.authFail(c, "That sign-in link could not be verified. Try again.")
		return
	}

	code := c.Query("code")
	if code == "" {
		h.authFail(c, "Google did not return an authorization code.")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	accessToken, err := h.exchangeCode(ctx, code)
	if err != nil {
		// A code Google will not take is almost always stale, reused, or from a
		// different client. That is something the person can retry, so it comes
		// back as a message on the app rather than an opaque 500.
		slog.Warn("google refused the code exchange", "err", err)
		h.authFail(c, "Google would not accept that sign-in. It may have expired — try again.")
		return
	}

	profile, err := h.fetchProfile(ctx, accessToken)
	if err != nil {
		slog.Warn("could not read the google profile", "err", err)
		h.authFail(c, "Your profile could not be read from Google. Try again.")
		return
	}

	user, err := h.db.UpsertUser(ctx, database.User{
		ID:            profile.Sub,
		Email:         profile.Email,
		EmailVerified: profile.EmailVerified,
		Name:          profile.Name,
		Picture:       profile.Picture,
	})
	if err != nil {
		h.fail(c, err)
		return
	}

	token, _, err := h.db.NewSession(ctx, user.ID, h.cfg.Session.Lifetime)
	if err != nil {
		h.fail(c, err)
		return
	}

	http.SetCookie(c.Writer, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/api",
		MaxAge:   int(h.cfg.Session.Lifetime.Seconds()),
		HttpOnly: true,
		Secure:   h.cfg.Session.Secure,
		SameSite: http.SameSiteLaxMode,
	})

	// No token in the URL, so it cannot end up in a history entry or a log.
	c.Redirect(http.StatusFound, h.frontendURL("/sites"))
}

func (h *Handler) exchangeCode(ctx context.Context, code string) (string, error) {
	form := url.Values{
		"code":          {code},
		"client_id":     {h.cfg.Google.ClientID},
		"client_secret": {h.cfg.Google.ClientSecret},
		"redirect_uri":  {h.cfg.Google.RedirectURI},
		"grant_type":    {"authorization_code"},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://oauth2.googleapis.com/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	body, err := doJSON(req)
	if err != nil {
		return "", err
	}

	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("read token response: %w", err)
	}
	if out.AccessToken == "" {
		// The description is Google's own text and safe to surface: it says why
		// a redirect URI or client id is wrong, which is the usual cause.
		return "", fmt.Errorf("google refused the exchange: %s %s", out.Error, out.Description)
	}
	return out.AccessToken, nil
}

type googleProfile struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
}

func (h *Handler) fetchProfile(ctx context.Context, accessToken string) (googleProfile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://www.googleapis.com/oauth2/v3/userinfo", nil)
	if err != nil {
		return googleProfile{}, fmt.Errorf("build profile request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	body, err := doJSON(req)
	if err != nil {
		return googleProfile{}, err
	}

	var p googleProfile
	if err := json.Unmarshal(body, &p); err != nil {
		return googleProfile{}, fmt.Errorf("read profile: %w", err)
	}
	if p.Sub == "" {
		return googleProfile{}, errors.New("google returned a profile with no subject")
	}
	return p, nil
}

// optionalAccount resolves a session when one is present and does nothing when
// it is not.
//
// The upload routes need this rather than requireAccount: an upload is allowed
// either way, it just becomes a temporary drop instead of an owned site. The
// alternative, reading the cookie by hand inside each handler, would let one
// forget to check.
func (h *Handler) optionalAccount(c *gin.Context) {
	cookie, err := c.Request.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		c.Next()
		return
	}

	user, err := h.db.SessionUser(c.Request.Context(), cookie.Value, time.Now())
	if err != nil {
		// An absent, expired or made-up session all mean the same thing here:
		// this is an anonymous upload. The cookie is not cleared, because a
		// stale one will simply fail again until the browser drops it.
		c.Next()
		return
	}

	c.Set(accountKey, account{User: user})
	c.Next()
}

// requireAccount rejects a request that carries no live session.
func (h *Handler) requireAccount(c *gin.Context) {
	cookie, err := c.Request.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Sign in to continue."})
		return
	}

	user, err := h.db.SessionUser(c.Request.Context(), cookie.Value, time.Now())
	if err != nil {
		if errors.Is(err, database.ErrNoSession) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Sign in to continue."})
			return
		}
		h.fail(c, err)
		return
	}

	c.Set(accountKey, account{User: user})
	c.Next()
}

// currentAccount returns the account requireAccount stored.
func currentAccount(c *gin.Context) account {
	a, ok := c.Get(accountKey)
	if !ok {
		return account{}
	}
	return a.(account)
}

func (h *Handler) me(c *gin.Context) {
	a := currentAccount(c)
	c.JSON(http.StatusOK, gin.H{
		"id":      a.User.ID,
		"email":   a.User.Email,
		"name":    a.User.Name,
		"picture": a.User.Picture,
	})
}

func (h *Handler) logout(c *gin.Context) {
	if cookie, err := c.Request.Cookie(sessionCookie); err == nil {
		if err := h.db.DeleteSession(c.Request.Context(), cookie.Value); err != nil {
			h.fail(c, err)
			return
		}
	}
	clearCookie(c, h.cfg, sessionCookie)
	c.JSON(http.StatusOK, gin.H{"status": "signed out"})
}

// authFail returns to the app rather than showing Google's error page, since
// these are conditions a person can retry.
func (h *Handler) authFail(c *gin.Context, message string) {
	c.Redirect(http.StatusFound, h.frontendURL("/sites?error="+url.QueryEscape(message)))
}

// frontendURL builds a link back to the app. Falls back to the control origin
// when FRONTEND_ORIGIN is unset, which is the same place sign-in started.
func (h *Handler) frontendURL(path string) string {
	if origin := strings.TrimSpace(h.cfg.FrontendOrigin); origin != "" {
		return strings.TrimSuffix(origin, "/") + path
	}
	return strings.TrimSuffix(h.cfg.PublicURL, "/") + path
}

func clearCookie(c *gin.Context, cfg config.Config, name string) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/api",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   cfg.Session.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func randomValue() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// doJSON sends a request and returns its body, turning a non-OK status into an
// error that includes Google's own explanation.
func doJSON(req *http.Request) ([]byte, error) {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call google: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read google response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}
