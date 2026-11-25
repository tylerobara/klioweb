// Package auth implements optional OIDC login with stateless signed-cookie sessions.
//
// The dashboard stays readable without logging in. Handlers that change
// anything must be wrapped in Auth.RequireLogin.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	sessionCookie = "klioweb_session"
	flowCookie    = "klioweb_oidc"
	flowTTL       = 10 * time.Minute
	// CSRFHeader must be present (any value) on state-changing requests.
	CSRFHeader = "X-Requested-With"
)

type Config struct {
	IssuerURL     string
	ClientID      string
	ClientSecret  string
	ExternalURL   string // public base URL of this app; callback is <ExternalURL>/auth/callback
	Scopes        []string
	GroupsClaim   string
	AllowedGroups []string // if set (or AllowedEmails), the user must match one
	AllowedEmails []string
	SessionTTL    time.Duration
	SessionSecret []byte
}

type User struct {
	Subject string `json:"subject"`
	Name    string `json:"name"`
	Email   string `json:"email,omitempty"`
}

type Auth struct {
	cfg      Config
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
	secure   bool

	// Version is reported to logged-in users by /api/me.
	Version string
}

// New validates cfg and discovers the provider, retrying briefly so the app
// can start before the identity provider is reachable.
func New(ctx context.Context, cfg Config) (*Auth, error) {
	if cfg.IssuerURL == "" || cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.ExternalURL == "" {
		return nil, errors.New("oidc: issuer URL, client ID, client secret and external URL are all required")
	}
	if len(cfg.SessionSecret) < 32 {
		return nil, errors.New("oidc: session secret must be at least 32 bytes")
	}
	ext, err := url.Parse(cfg.ExternalURL)
	if err != nil || (ext.Scheme != "http" && ext.Scheme != "https") || ext.Host == "" {
		return nil, fmt.Errorf("oidc: invalid external URL %q", cfg.ExternalURL)
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}
	if cfg.GroupsClaim == "" {
		cfg.GroupsClaim = "groups"
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 12 * time.Hour
	}

	var provider *oidc.Provider
	for attempt := 1; ; attempt++ {
		provider, err = oidc.NewProvider(ctx, cfg.IssuerURL)
		if err == nil {
			break
		}
		if attempt >= 6 || ctx.Err() != nil {
			return nil, fmt.Errorf("oidc discovery for %s: %w", cfg.IssuerURL, err)
		}
		log.Printf("oidc: discovery failed (%v), retrying", err)
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &Auth{
		cfg: cfg,
		oauth: &oauth2.Config{
			ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret,
			Endpoint:    provider.Endpoint(),
			RedirectURL: strings.TrimRight(cfg.ExternalURL, "/") + "/auth/callback",
			Scopes:      cfg.Scopes,
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		secure:   ext.Scheme == "https",
	}, nil
}

// ---- signed cookies -------------------------------------------------------

func (a *Auth) sign(v any) (string, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, a.cfg.SessionSecret)
	mac.Write(body)
	enc := base64.RawURLEncoding
	return enc.EncodeToString(body) + "." + enc.EncodeToString(mac.Sum(nil)), nil
}

func (a *Auth) unsign(token string, v any) bool {
	body, sig, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	enc := base64.RawURLEncoding
	raw, err1 := enc.DecodeString(body)
	got, err2 := enc.DecodeString(sig)
	if err1 != nil || err2 != nil {
		return false
	}
	mac := hmac.New(sha256.New, a.cfg.SessionSecret)
	mac.Write(raw)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return false
	}
	return json.Unmarshal(raw, v) == nil
}

func (a *Auth) setCookie(w http.ResponseWriter, name, value, path string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: path, MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode,
	})
}

func (a *Auth) clearCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: path, MaxAge: -1,
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode})
}

type session struct {
	User
	Exp int64 `json:"exp"`
}

type flow struct {
	State, Nonce, Verifier string
	Exp                    int64
}

func randString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// ---- handlers -------------------------------------------------------------

// Register mounts /auth/login, /auth/callback, /auth/logout and /api/me.
func (a *Auth) Register(mux *http.ServeMux) {
	mux.HandleFunc("/auth/login", a.login)
	mux.HandleFunc("/auth/callback", a.callback)
	mux.HandleFunc("/auth/logout", a.logout)
	mux.HandleFunc("/api/me", a.me)
}

// RegisterDisabled mounts /api/me reporting that login is not configured.
func RegisterDisabled(mux *http.ServeMux) {
	mux.HandleFunc("/api/me", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "authenticated": false})
	})
}

func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	f := flow{State: randString(24), Nonce: randString(24), Verifier: oauth2.GenerateVerifier(),
		Exp: time.Now().Add(flowTTL).Unix()}
	tok, err := a.sign(f)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.setCookie(w, flowCookie, tok, "/auth", flowTTL)
	http.Redirect(w, r, a.oauth.AuthCodeURL(f.State, oidc.Nonce(f.Nonce), oauth2.S256ChallengeOption(f.Verifier)),
		http.StatusFound)
}

func (a *Auth) callback(w http.ResponseWriter, r *http.Request) {
	fail := func(code int, msg string, err error) {
		if err != nil {
			log.Printf("auth: %s: %v", msg, err)
		}
		a.clearCookie(w, flowCookie, "/auth")
		http.Error(w, msg, code)
	}
	c, err := r.Cookie(flowCookie)
	var f flow
	if err != nil || !a.unsign(c.Value, &f) || time.Now().Unix() > f.Exp {
		fail(http.StatusBadRequest, "login session expired, please try again", err)
		return
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		fail(http.StatusForbidden, "login failed: "+e, nil)
		return
	}
	if !hmac.Equal([]byte(q.Get("state")), []byte(f.State)) {
		fail(http.StatusBadRequest, "state mismatch", nil)
		return
	}
	tok, err := a.oauth.Exchange(r.Context(), q.Get("code"), oauth2.VerifierOption(f.Verifier))
	if err != nil {
		fail(http.StatusBadGateway, "token exchange failed", err)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := a.verifier.Verify(r.Context(), raw)
	if err != nil {
		fail(http.StatusUnauthorized, "invalid id_token", err)
		return
	}
	if !hmac.Equal([]byte(idt.Nonce), []byte(f.Nonce)) {
		fail(http.StatusUnauthorized, "nonce mismatch", nil)
		return
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		fail(http.StatusUnauthorized, "unreadable claims", err)
		return
	}
	u, allowed := a.authorize(idt.Subject, claims)
	if !allowed {
		log.Printf("auth: %q (%s) authenticated but is not in the allow-list", u.Name, u.Email)
		fail(http.StatusForbidden, "you are not authorized to use this application", nil)
		return
	}
	val, err := a.sign(session{User: u, Exp: time.Now().Add(a.cfg.SessionTTL).Unix()})
	if err != nil {
		fail(http.StatusInternalServerError, "internal error", err)
		return
	}
	a.clearCookie(w, flowCookie, "/auth")
	a.setCookie(w, sessionCookie, val, "/", a.cfg.SessionTTL)
	http.Redirect(w, r, "/", http.StatusFound)
}

// authorize builds the User and applies the allow-lists. With no lists
// configured, any user the identity provider authenticates is allowed.
func (a *Auth) authorize(sub string, claims map[string]any) (User, bool) {
	str := func(k string) string { s, _ := claims[k].(string); return s }
	u := User{Subject: sub, Email: str("email")}
	for _, k := range []string{"name", "preferred_username", "email"} {
		if u.Name = str(k); u.Name != "" {
			break
		}
	}
	if u.Name == "" {
		u.Name = sub
	}
	if len(a.cfg.AllowedGroups) == 0 && len(a.cfg.AllowedEmails) == 0 {
		return u, true
	}
	for _, g := range groupList(claims[a.cfg.GroupsClaim]) {
		if contains(a.cfg.AllowedGroups, g) {
			return u, true
		}
	}
	// Only trust an email that the provider hasn't flagged as unverified.
	if verified, ok := claims["email_verified"].(bool); (!ok || verified) && u.Email != "" {
		for _, e := range a.cfg.AllowedEmails {
			if strings.EqualFold(e, u.Email) {
				return u, true
			}
		}
	}
	return u, false
}

func groupList(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{t}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.Header.Get(CSRFHeader) == "" {
		http.Error(w, "POST with "+CSRFHeader+" header required", http.StatusMethodNotAllowed)
		return
	}
	a.clearCookie(w, sessionCookie, "/")
	w.WriteHeader(http.StatusNoContent)
}

// User returns the logged-in user, if any.
func (a *Auth) User(r *http.Request) (*User, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, false
	}
	var s session
	if !a.unsign(c.Value, &s) || time.Now().Unix() > s.Exp {
		return nil, false
	}
	return &s.User, true
}

// UserName returns the logged-in user's display name.
func (a *Auth) UserName(r *http.Request) (string, bool) {
	u, ok := a.User(r)
	if !ok {
		return "", false
	}
	return u.Name, true
}

func (a *Auth) me(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if u, ok := a.User(r); ok {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "authenticated": true, "user": u, "version": a.Version})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "authenticated": false})
}

// RequireLogin guards state-changing handlers: it needs a valid session and,
// for anything other than GET/HEAD/OPTIONS, the CSRF header (which a
// cross-site form post cannot set).
func (a *Auth) RequireLogin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := a.User(r); !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "login required"})
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if r.Header.Get(CSRFHeader) == "" {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "missing " + CSRFHeader + " header"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
