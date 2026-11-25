package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// mockIdP is a minimal OIDC provider: discovery, JWKS and a token endpoint
// that enforces PKCE and signs an ID token carrying the nonce it was sent.
type mockIdP struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	claims map[string]any       // extra claims for the next ID token
	codes  map[string][2]string // code -> {nonce, code_challenge}
}

func newIdP(t *testing.T) *mockIdP {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	m := &mockIdP{key: key, codes: map[string][2]string{}, claims: map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": m.srv.URL, "authorization_endpoint": m.srv.URL + "/authorize",
			"token_endpoint": m.srv.URL + "/token", "jwks_uri": m.srv.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		info, ok := m.codes[r.Form.Get("code")]
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != info[1] {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
			(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
		std := jwt.Claims{Issuer: m.srv.URL, Subject: "user-1", Audience: jwt.Audience{"klioweb"},
			Expiry: jwt.NewNumericDate(time.Now().Add(time.Hour)), IssuedAt: jwt.NewNumericDate(time.Now())}
		extra := map[string]any{"nonce": info[0]}
		for k, v := range m.claims {
			extra[k] = v
		}
		idt, err := jwt.Signed(signer).Claims(std).Claims(extra).Serialize()
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": idt})
	})
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func newAuth(t *testing.T, m *mockIdP, mut func(*Config)) (*Auth, http.Handler) {
	cfg := Config{IssuerURL: m.srv.URL, ClientID: "klioweb", ClientSecret: "s3cret",
		ExternalURL: "https://klio.example.com", SessionSecret: []byte(strings.Repeat("k", 32)),
		SessionTTL: time.Hour}
	if mut != nil {
		mut(&cfg)
	}
	a, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	a.Register(mux)
	mux.Handle("/api/write", a.RequireLogin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})))
	return a, mux
}

func do(h http.Handler, method, target string, hdr map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func cookie(rr *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rr.Result().Cookies() {
		if c.Name == name && c.MaxAge >= 0 {
			return c
		}
	}
	return nil
}

// loginFlow plays the browser: /auth/login -> IdP -> /auth/callback.
func loginFlow(t *testing.T, m *mockIdP, h http.Handler, tamperState bool) *httptest.ResponseRecorder {
	t.Helper()
	rr := do(h, "GET", "/auth/login", nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("login status %d", rr.Code)
	}
	loc, _ := url.Parse(rr.Header().Get("Location"))
	q := loc.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" {
		t.Fatalf("login redirect lacks PKCE/nonce: %s", loc)
	}
	if q.Get("redirect_uri") != "https://klio.example.com/auth/callback" {
		t.Fatalf("redirect_uri = %s", q.Get("redirect_uri"))
	}
	m.codes["code-1"] = [2]string{q.Get("nonce"), q.Get("code_challenge")}
	state := q.Get("state")
	if tamperState {
		state = "forged"
	}
	fc := cookie(rr, flowCookie)
	if fc == nil || !fc.HttpOnly || !fc.Secure {
		t.Fatalf("flow cookie missing or insecure: %+v", fc)
	}
	return do(h, "GET", "/auth/callback?code=code-1&state="+state, nil, fc)
}

func TestFullLoginAndGuard(t *testing.T) {
	m := newIdP(t)
	m.claims = map[string]any{"name": "Tyler", "email": "t@example.com"}
	_, h := newAuth(t, m, nil)

	// Anonymous: me says not authenticated, writes are refused.
	if body := do(h, "GET", "/api/me", nil).Body.String(); !strings.Contains(body, `"authenticated":false`) || !strings.Contains(body, `"enabled":true`) {
		t.Fatalf("anon me: %s", body)
	}
	if rr := do(h, "POST", "/api/write", map[string]string{CSRFHeader: "x"}); rr.Code != 401 {
		t.Fatalf("anon write = %d, want 401", rr.Code)
	}

	rr := loginFlow(t, m, h, false)
	if rr.Code != http.StatusFound || rr.Header().Get("Location") != "/" {
		t.Fatalf("callback: %d %s %s", rr.Code, rr.Header().Get("Location"), rr.Body.String())
	}
	sc := cookie(rr, sessionCookie)
	if sc == nil || !sc.HttpOnly || !sc.Secure || sc.SameSite != http.SameSiteLaxMode {
		t.Fatalf("bad session cookie: %+v", sc)
	}

	if body := do(h, "GET", "/api/me", nil, sc).Body.String(); !strings.Contains(body, `"name":"Tyler"`) || !strings.Contains(body, `"authenticated":true`) {
		t.Fatalf("me: %s", body)
	}
	if rr := do(h, "POST", "/api/write", nil, sc); rr.Code != 403 {
		t.Fatalf("write without CSRF header = %d, want 403", rr.Code)
	}
	if rr := do(h, "POST", "/api/write", map[string]string{CSRFHeader: "x"}, sc); rr.Code != 202 {
		t.Fatalf("write = %d, want 202", rr.Code)
	}
	if rr := do(h, "GET", "/api/write", nil, sc); rr.Code != 202 {
		t.Fatalf("safe GET = %d, want 202", rr.Code)
	}

	// Tampered / foreign-signed cookies are rejected.
	bad := *sc
	bad.Value = sc.Value[:len(sc.Value)-3] + "AAA"
	if rr := do(h, "POST", "/api/write", map[string]string{CSRFHeader: "x"}, &bad); rr.Code != 401 {
		t.Fatalf("tampered cookie = %d, want 401", rr.Code)
	}
	other, _ := newAuth(t, m, func(c *Config) { c.SessionSecret = []byte(strings.Repeat("z", 32)) })
	forged, _ := other.sign(session{User: User{Name: "evil"}, Exp: time.Now().Add(time.Hour).Unix()})
	if rr := do(h, "GET", "/api/write", nil, &http.Cookie{Name: sessionCookie, Value: forged}); rr.Code != 401 {
		t.Fatalf("forged cookie = %d, want 401", rr.Code)
	}

	// Expired session.
	exp, _ := other.sign(session{Exp: time.Now().Add(-time.Minute).Unix()})
	a2, h2 := newAuth(t, m, func(c *Config) { c.SessionSecret = []byte(strings.Repeat("z", 32)) })
	_ = a2
	if rr := do(h2, "GET", "/api/write", nil, &http.Cookie{Name: sessionCookie, Value: exp}); rr.Code != 401 {
		t.Fatalf("expired session = %d, want 401", rr.Code)
	}

	// Logout needs POST + header and clears the cookie.
	if rr := do(h, "GET", "/auth/logout", nil, sc); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET logout = %d", rr.Code)
	}
	lo := do(h, "POST", "/auth/logout", map[string]string{CSRFHeader: "x"}, sc)
	if lo.Code != 204 {
		t.Fatalf("logout = %d", lo.Code)
	}
	for _, c := range lo.Result().Cookies() {
		if c.Name == sessionCookie && c.MaxAge >= 0 {
			t.Fatal("session cookie not cleared")
		}
	}
}

func TestStateMismatchAndPKCE(t *testing.T) {
	m := newIdP(t)
	_, h := newAuth(t, m, nil)
	if rr := loginFlow(t, m, h, true); rr.Code != 400 {
		t.Fatalf("forged state = %d, want 400", rr.Code)
	}
	// Wrong verifier: the IdP binds the code to the challenge from /auth/login.
	rr := do(h, "GET", "/auth/login", nil)
	loc, _ := url.Parse(rr.Header().Get("Location"))
	m.codes["code-1"] = [2]string{loc.Query().Get("nonce"), "challenge-of-someone-else"}
	r2 := do(h, "GET", "/auth/callback?code=code-1&state="+loc.Query().Get("state"), nil, cookie(rr, flowCookie))
	if r2.Code != http.StatusBadGateway {
		t.Fatalf("pkce failure = %d, want 502", r2.Code)
	}
	if cookie(r2, sessionCookie) != nil {
		t.Fatal("session issued on failed exchange")
	}
	// No flow cookie at all.
	if rr := do(h, "GET", "/auth/callback?code=x&state=y", nil); rr.Code != 400 {
		t.Fatalf("no flow cookie = %d, want 400", rr.Code)
	}
}

func TestAllowLists(t *testing.T) {
	cases := []struct {
		name   string
		cfg    func(*Config)
		claims map[string]any
		want   int
	}{
		{"no lists: anyone", nil, map[string]any{"name": "a"}, 302},
		{"group match", func(c *Config) { c.AllowedGroups = []string{"klio-admins"} },
			map[string]any{"groups": []string{"x", "klio-admins"}}, 302},
		{"group miss", func(c *Config) { c.AllowedGroups = []string{"klio-admins"} },
			map[string]any{"groups": []string{"x"}}, 403},
		{"custom groups claim", func(c *Config) { c.AllowedGroups = []string{"g"}; c.GroupsClaim = "roles" },
			map[string]any{"roles": []string{"g"}}, 302},
		{"email match", func(c *Config) { c.AllowedEmails = []string{"T@Example.com"} },
			map[string]any{"email": "t@example.com", "email_verified": true}, 302},
		{"email unverified", func(c *Config) { c.AllowedEmails = []string{"t@example.com"} },
			map[string]any{"email": "t@example.com", "email_verified": false}, 403},
		{"email miss", func(c *Config) { c.AllowedEmails = []string{"t@example.com"} },
			map[string]any{"email": "other@example.com"}, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newIdP(t)
			m.claims = tc.claims
			_, h := newAuth(t, m, tc.cfg)
			rr := loginFlow(t, m, h, false)
			if rr.Code != tc.want {
				t.Fatalf("status %d, want %d (%s)", rr.Code, tc.want, rr.Body.String())
			}
			if (tc.want == 403) == (cookie(rr, sessionCookie) != nil) {
				t.Fatalf("session cookie presence wrong for status %d", rr.Code)
			}
		})
	}
}

func TestConfigValidation(t *testing.T) {
	m := newIdP(t)
	for name, mut := range map[string]func(*Config){
		"short secret": func(c *Config) { c.SessionSecret = []byte("short") },
		"no client id": func(c *Config) { c.ClientID = "" },
		"bad ext url":  func(c *Config) { c.ExternalURL = "klio.example.com" },
	} {
		cfg := Config{IssuerURL: m.srv.URL, ClientID: "klioweb", ClientSecret: "s", ExternalURL: "https://k.example",
			SessionSecret: []byte(strings.Repeat("k", 32))}
		mut(&cfg)
		if _, err := New(context.Background(), cfg); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
