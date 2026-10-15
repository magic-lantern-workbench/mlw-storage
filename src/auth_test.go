package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testIssuer = "http://localhost:8180/realms/mlw"

type testKey struct {
	kid string
	key *rsa.PrivateKey
}

func newTestKey(t *testing.T, kid string) testKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return testKey{kid: kid, key: k}
}

func (k testKey) jwk() map[string]string {
	return map[string]string{
		"kid": k.kid, "kty": "RSA", "use": "sig", "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(k.key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.key.E)).Bytes()),
	}
}

// jwksServer serves the given keys (swappable) and counts requests.
type jwksServer struct {
	*httptest.Server
	keys atomic.Pointer[[]testKey]
	hits atomic.Int32
}

func newJWKSServer(t *testing.T, keys ...testKey) *jwksServer {
	t.Helper()
	s := &jwksServer{}
	s.keys.Store(&keys)
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		var jwks []map[string]string
		for _, k := range *s.keys.Load() {
			jwks = append(jwks, k.jwk())
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": jwks})
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *jwksServer) setKeys(keys ...testKey) { s.keys.Store(&keys) }

// claimsMap returns valid Keycloak-like access token claims.
func claimsMap(sub string, roles ...string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":                testIssuer,
		"sub":                sub,
		"typ":                "Bearer",
		"azp":                "mlw-app",
		"preferred_username": "mlw",
		"exp":                time.Now().Add(5 * time.Minute).Unix(),
		"iat":                time.Now().Unix(),
		"realm_access":       map[string]any{"roles": roles},
	}
}

func sign(t *testing.T, k testKey, c jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	tok.Header["kid"] = k.kid
	s, err := tok.SignedString(k.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newTestVerifier(jwksURL string, mutate func(*authConfig)) *verifier {
	cfg := authConfig{
		Issuer:       testIssuer,
		JWKSURL:      jwksURL,
		RequiredRole: "mlw-user",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return newVerifier(cfg)
}

func TestVerifyValidToken(t *testing.T) {
	k := newTestKey(t, "k1")
	srv := newJWKSServer(t, k)
	v := newTestVerifier(srv.URL, nil)

	c, err := v.verify(context.Background(), sign(t, k, claimsMap("user-1", "mlw-user", "other")))
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != "user-1" || c.AZP != "mlw-app" || c.PreferredUsername != "mlw" {
		t.Errorf("claims = %+v", c)
	}
	if !c.hasRole("mlw-user") || !c.hasRole("other") || c.hasRole("admin") {
		t.Errorf("roles = %v", c.RealmAccess.Roles)
	}
}

func TestVerifyRejects(t *testing.T) {
	k := newTestKey(t, "k1")
	other := newTestKey(t, "k1") // same kid, different key material
	srv := newJWKSServer(t, k)

	mod := func(f func(jwt.MapClaims)) jwt.MapClaims {
		c := claimsMap("user-1", "mlw-user")
		f(c)
		return c
	}

	tests := []struct {
		name   string
		token  func() string
		mutate func(*authConfig)
	}{
		{"wrong issuer", func() string {
			return sign(t, k, mod(func(c jwt.MapClaims) { c["iss"] = "http://evil/realms/mlw" }))
		}, nil},
		{"expired", func() string {
			return sign(t, k, mod(func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Hour).Unix() }))
		}, nil},
		{"no expiry", func() string {
			return sign(t, k, mod(func(c jwt.MapClaims) { delete(c, "exp") }))
		}, nil},
		{"no subject", func() string {
			return sign(t, k, mod(func(c jwt.MapClaims) { delete(c, "sub") }))
		}, nil},
		{"id token, not access token", func() string {
			return sign(t, k, mod(func(c jwt.MapClaims) { c["typ"] = "ID" }))
		}, nil},
		{"signed by another key", func() string {
			return sign(t, other, claimsMap("user-1", "mlw-user"))
		}, nil},
		{"unknown kid", func() string {
			return sign(t, testKey{kid: "nope", key: k.key}, claimsMap("user-1", "mlw-user"))
		}, nil},
		{"client not allowed", func() string {
			return sign(t, k, claimsMap("user-1", "mlw-user"))
		}, func(c *authConfig) { c.AllowedClients = []string{"someone-else"} }},
		{"alg none", func() string {
			tok := jwt.NewWithClaims(jwt.SigningMethodNone, claimsMap("user-1", "mlw-user"))
			tok.Header["kid"] = "k1"
			s, _ := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
			return s
		}, nil},
		{"HS256 signed with the public key (alg confusion)", func() string {
			tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claimsMap("user-1", "mlw-user"))
			tok.Header["kid"] = "k1"
			s, _ := tok.SignedString([]byte(base64.RawURLEncoding.EncodeToString(k.key.N.Bytes())))
			return s
		}, nil},
		{"garbage", func() string { return "a.b.c" }, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newTestVerifier(srv.URL, tt.mutate)
			if _, err := v.verify(context.Background(), tt.token()); err == nil {
				t.Error("token accepted, want rejection")
			}
		})
	}
}

func TestVerifyAllowedClient(t *testing.T) {
	k := newTestKey(t, "k1")
	srv := newJWKSServer(t, k)
	v := newTestVerifier(srv.URL, func(c *authConfig) { c.AllowedClients = []string{"a", "mlw-app"} })
	if _, err := v.verify(context.Background(), sign(t, k, claimsMap("u", "mlw-user"))); err != nil {
		t.Errorf("allowed client rejected: %v", err)
	}
}

func TestKeySetRefreshesOnRotation(t *testing.T) {
	old := newTestKey(t, "old")
	srv := newJWKSServer(t, old)
	ks := newKeySet(srv.URL)
	ks.client = srv.Client()

	if _, err := ks.key(context.Background(), "old"); err != nil {
		t.Fatal(err)
	}
	if _, err := ks.key(context.Background(), "old"); err != nil {
		t.Fatal(err)
	}
	if got := srv.hits.Load(); got != 1 {
		t.Errorf("jwks fetched %d times for a cached key, want 1", got)
	}

	// Rotation: a new kid appears. Within the refresh window it is not fetched.
	fresh := newTestKey(t, "new")
	srv.setKeys(fresh)
	if _, err := ks.key(context.Background(), "new"); err == nil {
		t.Error("unknown kid within refresh window should fail without refetching")
	}
	if got := srv.hits.Load(); got != 1 {
		t.Errorf("jwks refetched within window: %d fetches", got)
	}

	// After the window, the unknown kid triggers one refresh and is found.
	ks.mu.Lock()
	ks.fetched = time.Now().Add(-time.Minute)
	ks.mu.Unlock()
	if _, err := ks.key(context.Background(), "new"); err != nil {
		t.Errorf("rotated key not found after refresh: %v", err)
	}
	if got := srv.hits.Load(); got != 2 {
		t.Errorf("jwks fetched %d times, want 2", got)
	}
}

func TestKeySetUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := newKeySet(srv.URL).key(context.Background(), "k1"); err == nil {
		t.Error("expected error when the JWKS endpoint fails")
	}
}

func callHook(t *testing.T, h http.Handler, method string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	switch b := body.(type) {
	case string:
		buf.WriteString(b)
	default:
		_ = json.NewEncoder(&buf).Encode(b)
	}
	req := httptest.NewRequest(method, "/auth", &buf)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeUser(t *testing.T, rec *httptest.ResponseRecorder) hookUser {
	t.Helper()
	var u hookUser
	if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return u
}

func TestAuthHook(t *testing.T) {
	k := newTestKey(t, "k1")
	srv := newJWKSServer(t, k)
	const sub = "6b1f7c1e-0000-4000-8000-000000000001"

	login := func(user, password string) map[string]string {
		return map[string]string{"username": user, "password": password, "protocol": "SSH", "ip": "10.0.0.1"}
	}

	t.Run("valid token provisions user", func(t *testing.T) {
		h := authHook(newTestVerifier(srv.URL, nil), testStorage(t))
		rec := callHook(t, h, http.MethodPost, login(sub, sign(t, k, claimsMap(sub, "mlw-user"))))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body)
		}
		u := decodeUser(t, rec)
		if u.Username != sub || u.Status != 1 {
			t.Errorf("user = %+v", u)
		}
		if u.HomeDir != "/srv/sftpgo/users/"+sub {
			t.Errorf("home = %q", u.HomeDir)
		}
		if got := u.Permissions["/"]; len(got) != 1 || got[0] != "*" {
			t.Errorf("permissions = %v, want full access", got)
		}
	})

	t.Run("read-only role limits permissions", func(t *testing.T) {
		h := authHook(newTestVerifier(srv.URL, func(c *authConfig) { c.ReadOnlyRole = "mlw-readonly" }), testStorage(t))
		rec := callHook(t, h, http.MethodPost, login(sub, sign(t, k, claimsMap(sub, "mlw-user", "mlw-readonly"))))
		u := decodeUser(t, rec)
		if got := strings.Join(u.Permissions["/"], ","); got != "list,download" {
			t.Errorf("permissions = %q, want list,download", got)
		}
	})

	t.Run("username must match sub", func(t *testing.T) {
		h := authHook(newTestVerifier(srv.URL, nil), testStorage(t))
		rec := callHook(t, h, http.MethodPost, login("someone-else", sign(t, k, claimsMap(sub, "mlw-user"))))
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("required role missing", func(t *testing.T) {
		h := authHook(newTestVerifier(srv.URL, nil), testStorage(t))
		rec := callHook(t, h, http.MethodPost, login(sub, sign(t, k, claimsMap(sub, "unrelated"))))
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("no required role configured", func(t *testing.T) {
		h := authHook(newTestVerifier(srv.URL, func(c *authConfig) { c.RequiredRole = "" }), testStorage(t))
		rec := callHook(t, h, http.MethodPost, login(sub, sign(t, k, claimsMap(sub))))
		if rec.Code != http.StatusOK || decodeUser(t, rec).Username != sub {
			t.Errorf("status = %d body %s", rec.Code, rec.Body)
		}
	})

	t.Run("invalid token is rejected, not passed through", func(t *testing.T) {
		h := authHook(newTestVerifier(srv.URL, nil), testStorage(t))
		expired := claimsMap(sub, "mlw-user")
		expired["exp"] = time.Now().Add(-time.Hour).Unix()
		rec := callHook(t, h, http.MethodPost, login(sub, sign(t, k, expired)))
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("non-token password is rejected", func(t *testing.T) {
		// A 200 with an empty body would make SFTPGo accept any password, so
		// anything that is not a valid token must be a 403.
		h := authHook(newTestVerifier(srv.URL, nil), testStorage(t))
		for _, pw := range []string{"hunter2", "", "has.one.dot.too.many", "a b.c.d"} {
			rec := callHook(t, h, http.MethodPost, login("local", pw))
			if rec.Code != http.StatusForbidden {
				t.Errorf("password %q: status = %d, want 403", pw, rec.Code)
			}
			if rec.Body.Len() > 0 && strings.Contains(rec.Body.String(), `"username"`) {
				t.Errorf("password %q: response names a user: %s", pw, rec.Body)
			}
		}
	})

	t.Run("method and body validation", func(t *testing.T) {
		h := authHook(newTestVerifier(srv.URL, nil), testStorage(t))
		if rec := callHook(t, h, http.MethodGet, ""); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET status = %d, want 405", rec.Code)
		}
		if rec := callHook(t, h, http.MethodPost, "{not json"); rec.Code != http.StatusBadRequest {
			t.Errorf("bad body status = %d, want 400", rec.Code)
		}
	})
}

func TestHelpers(t *testing.T) {
	if !looksLikeJWT("a.b.c") || looksLikeJWT("a.b") || looksLikeJWT("a.b.c.d") || looksLikeJWT("a b.c.d") {
		t.Error("looksLikeJWT misclassified an input")
	}
	got := splitList(" a, b ,,c ")
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("splitList = %q", got)
	}
	if len(splitList("")) != 0 {
		t.Error("splitList(\"\") should be empty")
	}
	if !contains([]string{"a", "b"}, "b") || contains([]string{"a"}, "z") || contains(nil, "a") {
		t.Error("contains misbehaves")
	}
}

// testStorage returns a storage rooted in a temp dir with the same users dir
// the hook tests expect.
func testStorage(t *testing.T) *storage {
	t.Helper()
	st, err := newStorage("/srv/sftpgo/users", t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	return st
}
