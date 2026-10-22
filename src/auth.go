// COPYRIGHT_BEGIN
// MIT License
//
// Copyright (c) 2026 Wizzer Works
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.
// COPYRIGHT_END

package main

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// authConfig configures Keycloak (OIDC) token authentication. It is enabled
// when Issuer is set.
type authConfig struct {
	Issuer         string   // expected "iss", e.g. http://localhost:8180/realms/mlw
	JWKSURL        string   // where to fetch the realm's signing keys
	AllowedClients []string // accepted "azp" values; empty accepts any client
	RequiredRole   string   // realm role needed to log in; empty requires none
	ReadOnlyRole   string   // realm role that limits a user to list/download
}

// keySet fetches and caches the RSA signing keys published at a JWKS URL.
type keySet struct {
	url    string
	client *http.Client

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

const jwksMinRefresh = 10 * time.Second

func newKeySet(url string) *keySet {
	return &keySet{url: url, client: &http.Client{Timeout: 10 * time.Second}}
}

// key returns the public key for kid, refreshing the cache when the kid is
// unknown (key rotation), but at most once per jwksMinRefresh.
func (k *keySet) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if key, ok := k.keys[kid]; ok {
		return key, nil
	}
	if k.keys != nil && time.Since(k.fetched) < jwksMinRefresh {
		return nil, fmt.Errorf("unknown signing key %q", kid)
	}
	if err := k.refresh(ctx); err != nil {
		return nil, err
	}
	if key, ok := k.keys[kid]; ok {
		return key, nil
	}
	return nil, fmt.Errorf("unknown signing key %q", kid)
}

func (k *keySet) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		return err
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch jwks: status %d", resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return fmt.Errorf("decode jwks: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, jk := range doc.Keys {
		if jk.Kty != "RSA" || (jk.Use != "" && jk.Use != "sig") {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(jk.N)
		e, err2 := base64.RawURLEncoding.DecodeString(jk.E)
		if err1 != nil || err2 != nil || len(n) == 0 || len(e) == 0 {
			continue
		}
		keys[jk.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	k.keys, k.fetched = keys, time.Now()
	return nil
}

// tokenClaims are the Keycloak access token claims the gateway uses.
type tokenClaims struct {
	jwt.RegisteredClaims
	Typ               string `json:"typ"`
	AZP               string `json:"azp"`
	PreferredUsername string `json:"preferred_username"`
	Name              string `json:"name"`
	GivenName         string `json:"given_name"`
	FamilyName        string `json:"family_name"`
	Email             string `json:"email"`
	RealmAccess       struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

// description is what the SFTPGo user's Description field shows, so an
// administrator can tell whose directory a user id is: "Name <email>", or
// whichever of the two the token carries (nothing if it carries neither).
func (c *tokenClaims) description() string {
	name := strings.TrimSpace(c.Name)
	if name == "" {
		name = strings.TrimSpace(c.GivenName + " " + c.FamilyName)
	}
	switch email := strings.TrimSpace(c.Email); {
	case name != "" && email != "":
		return name + " <" + email + ">"
	case name != "":
		return name
	default:
		return email
	}
}

func (c *tokenClaims) hasRole(role string) bool {
	for _, r := range c.RealmAccess.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// verifier validates Keycloak access tokens.
type verifier struct {
	cfg  authConfig
	keys *keySet
}

func newVerifier(cfg authConfig) *verifier {
	return &verifier{cfg: cfg, keys: newKeySet(cfg.JWKSURL)}
}

// verify checks the signature (RS256 only), issuer, expiry, token type and
// allowed client, and returns the claims.
func (v *verifier) verify(ctx context.Context, raw string) (*tokenClaims, error) {
	claims := &tokenClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("token has no kid")
		}
		return v.keys.key(ctx, kid)
	},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(v.cfg.Issuer),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(30*time.Second),
	)
	if err != nil {
		return nil, err
	}
	if claims.Subject == "" {
		return nil, errors.New("token has no sub")
	}
	if claims.Typ != "Bearer" {
		return nil, fmt.Errorf("token type %q is not an access token", claims.Typ)
	}
	if len(v.cfg.AllowedClients) > 0 && !contains(v.cfg.AllowedClients, claims.AZP) {
		return nil, fmt.Errorf("client %q is not allowed", claims.AZP)
	}
	return claims, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// looksLikeJWT reports whether s has the three dot-separated parts of a JWT.
func looksLikeJWT(s string) bool {
	return strings.Count(s, ".") == 2 && !strings.ContainsAny(s, " \t\r\n")
}

// SFTPGo external authentication hook payloads (only the fields used).
type hookRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Protocol string `json:"protocol"`
	IP       string `json:"ip"`
}

type hookUser struct {
	Status      int                 `json:"status"`
	Username    string              `json:"username"`
	HomeDir     string              `json:"home_dir"`
	Description string              `json:"description,omitempty"`
	Permissions map[string][]string `json:"permissions"`
	// Shared drives for this user. SFTPGo replaces the user's folders with
	// what the hook returns, so they must be repeated at every login.
	Folders []driveGrant `json:"virtual_folders,omitempty"`
}

// driveGranter says which shared drives a user may use (see mediaStore).
type driveGranter interface {
	grants(user string, roles []string) []driveGrant
}

// authHook returns the handler SFTPGo calls to authenticate password logins.
//
// The password must be a Keycloak access token; the login name must equal the
// token's sub (Keycloak user id) and the user must hold the required realm
// role. Everything else is rejected, so while the hook is enabled SFTPGo's
// local users can only log in with public keys.
func authHook(v *verifier, st *storage, drives ...driveGranter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req hookRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// Fail closed. Never answer 200 with an empty body here: SFTPGo reads
		// that as "the hook already authenticated the user" and accepts any
		// password. There is no safe fallback to SFTPGo's own password check.
		if !looksLikeJWT(req.Password) {
			log.Printf("auth: reject %s login %q from %s: password is not a Keycloak access token", req.Protocol, req.Username, req.IP)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		claims, err := v.verify(r.Context(), req.Password)
		if err != nil {
			log.Printf("auth: reject %s login %q from %s: %v", req.Protocol, req.Username, req.IP, err)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if req.Username != claims.Subject {
			log.Printf("auth: reject %s login %q from %s: username does not match token sub", req.Protocol, req.Username, req.IP)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if v.cfg.RequiredRole != "" && !claims.hasRole(v.cfg.RequiredRole) {
			log.Printf("auth: reject %s login %q from %s: missing role %q", req.Protocol, req.Username, req.IP, v.cfg.RequiredRole)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		perms := []string{"*"}
		if v.cfg.ReadOnlyRole != "" && claims.hasRole(v.cfg.ReadOnlyRole) {
			perms = []string{"list", "download"}
		}
		home, err := st.home(claims.Subject)
		if err != nil {
			log.Printf("auth: reject %s login %q from %s: %v", req.Protocol, req.Username, req.IP, err)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		log.Printf("auth: accept %s login for user %s (%s) from %s, root %s", req.Protocol, claims.Subject, claims.PreferredUsername, req.IP, home)
		out := hookUser{
			Status:      1,
			Username:    claims.Subject,
			HomeDir:     home,
			Description: claims.description(),
			Permissions: map[string][]string{"/": perms},
		}
		for _, d := range drives {
			for _, g := range d.grants(claims.Subject, claims.RealmAccess.Roles) {
				out.Folders = append(out.Folders, g)
				if g.ReadOnly { // otherwise the drive inherits the user's permissions for /
					out.Permissions[g.VirtualPath] = []string{"list", "download"}
				}
			}
		}
		writeJSON(w, http.StatusOK, out)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
