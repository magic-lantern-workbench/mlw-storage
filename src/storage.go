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
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// storage decides which directory a Keycloak user sees as their root.
//
// By default a user gets a private directory <usersDir>/<sub>. A user can
// instead point their root at an existing directory under sharedDir (for
// example to open files that already exist), through the HTTP API below. The
// choice is stored by Keycloak user id and applied at every login.
//
// The gateway sees the same paths as SFTPGo (the data directory is mounted
// read-only at the same location), so it can validate directories itself.
type storage struct {
	usersDir  string // private per-user homes, <usersDir>/<sub>
	sharedDir string // the only tree users may select a root from
	statePath string // JSON file holding the selections; "" keeps them in memory

	mu       sync.Mutex
	selected map[string]string // sub -> path relative to sharedDir
}

func newStorage(usersDir, sharedDir, statePath string) (*storage, error) {
	s := &storage{
		usersDir:  usersDir,
		sharedDir: filepath.Clean(sharedDir),
		statePath: statePath,
		selected:  map[string]string{},
	}
	if statePath == "" {
		return s, nil
	}
	b, err := os.ReadFile(statePath)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", statePath, err)
	default:
		if err := json.Unmarshal(b, &s.selected); err != nil {
			return nil, fmt.Errorf("parse %s: %w", statePath, err)
		}
	}
	return s, nil
}

var (
	errSharedMissing = errors.New("shared directory not found")
	errBadPath       = errors.New("invalid path")
)

// resolve validates rel (a path relative to sharedDir, "" meaning sharedDir
// itself) and returns the real absolute directory. It rejects absolute paths,
// "..", anything that is not an existing directory, and symlinks that lead
// outside sharedDir.
func (s *storage) resolve(rel string) (string, error) {
	if strings.ContainsRune(rel, 0) || path.IsAbs(rel) || filepath.IsAbs(rel) {
		return "", errBadPath
	}
	clean := path.Clean(filepath.ToSlash(rel))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errBadPath
	}
	base, err := filepath.EvalSymlinks(s.sharedDir)
	if err != nil {
		return "", errSharedMissing
	}
	real, err := filepath.EvalSymlinks(filepath.Join(base, filepath.FromSlash(clean)))
	if err != nil {
		return "", fmt.Errorf("%w: no such directory", errBadPath)
	}
	if real != base && !strings.HasPrefix(real, base+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: outside the shared directory", errBadPath)
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%w: not a directory", errBadPath)
	}
	return real, nil
}

// relPath returns the stored selection for sub ("" and false if none).
func (s *storage) relPath(sub string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rel, ok := s.selected[sub]
	return rel, ok
}

// home returns the directory SFTPGo should use as sub's root. A selected
// directory is re-validated on every call and an error is returned if it has
// disappeared, so files are never silently written somewhere else.
func (s *storage) home(sub string) (string, error) {
	rel, ok := s.relPath(sub)
	if !ok {
		return path.Join(s.usersDir, sub), nil
	}
	dir, err := s.resolve(rel)
	if err != nil {
		return "", fmt.Errorf("selected directory %q: %w", rel, err)
	}
	return dir, nil
}

// set records rel as sub's root after validating it.
func (s *storage) set(sub, rel string) (string, error) {
	dir, err := s.resolve(rel)
	if err != nil {
		return "", err
	}
	clean := path.Clean(filepath.ToSlash(rel))
	if clean == "." {
		clean = ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.selected[sub]
	s.selected[sub] = clean
	if err := s.saveLocked(); err != nil {
		if had {
			s.selected[sub] = prev
		} else {
			delete(s.selected, sub)
		}
		return "", err
	}
	return dir, nil
}

// clear removes sub's selection, returning them to their private directory.
func (s *storage) clear(sub string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.selected[sub]
	if !had {
		return nil
	}
	delete(s.selected, sub)
	if err := s.saveLocked(); err != nil {
		s.selected[sub] = prev
		return err
	}
	return nil
}

func (s *storage) saveLocked() error {
	if s.statePath == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.selected, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.statePath)
}

// dirs lists the names of the subdirectories of rel (relative to sharedDir).
func (s *storage) dirs(rel string) ([]string, error) {
	dir, err := s.resolve(rel)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, e := range entries {
		// Follow symlinks only if they stay inside the shared directory.
		if _, err := s.resolve(path.Join(filepath.ToSlash(rel), e.Name())); err == nil {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

type rootResponse struct {
	Sub  string `json:"sub"`
	Path string `json:"path"` // relative to the shared directory; "" if none selected
	// Selected is true when the user chose a directory; false means the
	// private default directory is in use.
	Selected bool   `json:"selected"`
	Home     string `json:"home"` // directory the SFTP session starts in
}

// storageAPI serves the endpoints a user (through app1) uses to choose which
// existing directory is their SFTP root. Every call needs a valid Keycloak
// access token for a user holding the required role.
//
//	GET    {prefix}root        current selection
//	PUT    {prefix}root        {"path": "projects/a"}  select a directory
//	DELETE {prefix}root        back to the private default directory
//	GET    {prefix}dirs?path=  list subdirectories that can be selected
//	GET    {prefix}drives      the removable drives the user may use now
func storageAPI(prefix string, v *verifier, st *storage, drives ...driveGranter) http.Handler {
	mux := http.NewServeMux()

	current := func(sub string) rootResponse {
		rel, sel := st.relPath(sub)
		home, err := st.home(sub)
		if err != nil {
			home = ""
		}
		return rootResponse{Sub: sub, Path: rel, Selected: sel, Home: home}
	}

	mux.HandleFunc(prefix+"root", func(w http.ResponseWriter, r *http.Request) {
		claims, ok := authorize(w, r, v)
		if !ok {
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, current(claims.Subject))
		case http.MethodPut:
			var body struct {
				Path string `json:"path"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
				writeError(w, http.StatusBadRequest, "body must be JSON like {\"path\": \"projects/a\"}")
				return
			}
			if _, err := st.set(claims.Subject, body.Path); err != nil {
				status := http.StatusBadRequest
				switch {
				case errors.Is(err, errSharedMissing):
					status = http.StatusServiceUnavailable
				case !errors.Is(err, errBadPath):
					log.Printf("storage: set root for %s: %v", claims.Subject, err)
					status = http.StatusInternalServerError
				}
				writeError(w, status, err.Error())
				return
			}
			log.Printf("storage: user %s root set to %q", claims.Subject, body.Path)
			writeJSON(w, http.StatusOK, current(claims.Subject))
		case http.MethodDelete:
			if err := st.clear(claims.Subject); err != nil {
				log.Printf("storage: clear root for %s: %v", claims.Subject, err)
				writeError(w, http.StatusInternalServerError, "could not save")
				return
			}
			log.Printf("storage: user %s root reset to default", claims.Subject)
			writeJSON(w, http.StatusOK, current(claims.Subject))
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	})

	mux.HandleFunc(prefix+"dirs", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := authorize(w, r, v); !ok {
			return
		}
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		rel := r.URL.Query().Get("path")
		names, err := st.dirs(rel)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, errSharedMissing) {
				status = http.StatusServiceUnavailable
			}
			writeError(w, status, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": rel, "dirs": names})
	})

	// The removable drives (see media_share.go) the user can use now: those the
	// administrator enabled for the user or one of the token's realm roles and
	// that are mounted. They appear in the user's SFTP root at path.
	mux.HandleFunc(prefix+"drives", func(w http.ResponseWriter, r *http.Request) {
		claims, ok := authorize(w, r, v)
		if !ok {
			return
		}
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		userReadOnly := v.cfg.ReadOnlyRole != "" && claims.hasRole(v.cfg.ReadOnlyRole)
		list := []driveInfo{}
		for _, d := range drives {
			for _, g := range d.grants(claims.Subject, claims.RealmAccess.Roles) {
				list = append(list, driveInfo{Name: strings.TrimPrefix(g.VirtualPath, "/drives/"), Path: g.VirtualPath, ReadOnly: g.ReadOnly || userReadOnly})
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"drives": list})
	})

	return mux
}

// driveInfo is one entry of the drives answer.
type driveInfo struct {
	Name     string `json:"name"`
	Path     string `json:"path"`      // where it appears in the user's storage, /drives/<name>
	ReadOnly bool   `json:"read_only"` // the drive is read-only, or the user is
}

// authorize validates the bearer token and the required role, writing the
// error response itself when it fails.
func authorize(w http.ResponseWriter, r *http.Request, v *verifier) (*tokenClaims, bool) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || raw == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="mlw-storage"`)
		writeError(w, http.StatusUnauthorized, "missing bearer token")
		return nil, false
	}
	claims, err := v.verify(r.Context(), raw)
	if err != nil {
		log.Printf("storage api: reject %s %s: %v", r.Method, r.URL.Path, err)
		w.Header().Set("WWW-Authenticate", `Bearer realm="mlw-storage", error="invalid_token"`)
		writeError(w, http.StatusUnauthorized, "invalid token")
		return nil, false
	}
	if v.cfg.RequiredRole != "" && !claims.hasRole(v.cfg.RequiredRole) {
		writeError(w, http.StatusForbidden, "missing required role")
		return nil, false
	}
	return claims, true
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
