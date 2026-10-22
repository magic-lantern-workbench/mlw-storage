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

package main

// Publishing an enabled drive through SFTPGo. The gateway keeps, per drive, who
// may use it (SFTPGo users and Keycloak realm roles) and whether it is
// read-only. The SFTPGo objects themselves (a virtual folder, and its link to
// each chosen user) are managed through the SFTPGo API in drive_access.go.
//
// Users who sign in with a Keycloak token are rebuilt by the authentication
// hook at every login, which erases anything attached to them in SFTPGo, so the
// hook asks grants for the folders to add back.

import (
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var slugBadRE = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// slugFor derives a short path-safe name for a drive, unique among the shares.
// Call with s.mu held.
func (s *mediaStore) slugFor(key, name string) string {
	base := strings.Trim(slugBadRE.ReplaceAllString(name, "-"), "-.")
	if len(base) > 40 {
		base = strings.Trim(base[:40], "-.")
	}
	if base == "" {
		base = "drive"
	}
	taken := map[string]bool{}
	for k, kn := range s.known {
		if k != key && kn.Share != nil {
			taken[strings.ToLower(kn.Share.Slug)] = true
		}
	}
	slug := base
	for i := 2; taken[strings.ToLower(slug)]; i++ {
		slug = base + "-" + strconv.Itoa(i)
	}
	return slug
}

// prepareShare works out how the mounted drive key would be published, without
// recording anything. The caller makes SFTPGo match, then calls commitShare.
func (s *mediaStore) prepareShare(key string, users, roles []string, readOnly bool) (share, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.mounted[key]
	if !ok || s.cfg.SFTPGoDir == "" {
		return share{}, errNoSuchDrive
	}
	sh := share{Users: users, Roles: roles, ReadOnly: readOnly,
		Mapped: s.cfg.SFTPGoDir + strings.TrimPrefix(d.MountPoint, s.cfg.HostDir)}
	if kn := s.known[key]; kn.Share != nil {
		sh.Slug = kn.Share.Slug
	} else {
		sh.Slug = s.slugFor(key, d.Name)
	}
	return sh, nil
}

// commitShare marks the drive available and remembers its share.
func (s *mediaStore) commitShare(key string, sh share, admin string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.mounted[key]
	if !ok {
		return errNoSuchDrive
	}
	kn := s.newKnown(d, s.known[key], driveAvailable, admin)
	kn.Share = &sh
	s.known[key] = kn
	if err := s.save(); err != nil {
		return err
	}
	log.Printf("media: %q published at %s for %d user(s), %d role(s), read-only=%v by %q", d.Name, sh.VPath(), len(sh.Users), len(sh.Roles), sh.ReadOnly, admin)
	s.notifyLocked()
	return nil
}

// shareOf returns the share of a drive, if it has one.
func (s *mediaStore) shareOf(key string) (share, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kn, ok := s.known[key]; ok && kn.Share != nil {
		return *kn.Share, true
	}
	return share{}, false
}

// driveGrant is a folder to give a user at sign-in.
type driveGrant struct {
	Name        string `json:"name"`
	MappedPath  string `json:"mapped_path"`
	VirtualPath string `json:"virtual_path"`
	ReadOnly    bool   `json:"-"`
}

// grants lists the shared drives a user may use now: the drive must be
// enabled, mounted, and name the user or one of the roles.
func (s *mediaStore) grants(user string, roles []string) []driveGrant {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []driveGrant
	for k, kn := range s.known {
		sh := kn.Share
		if sh == nil || kn.State != driveAvailable {
			continue
		}
		if _, here := s.mounted[k]; !here {
			continue
		}
		if !contains(sh.Users, user) && !anyIn(sh.Roles, roles) {
			continue
		}
		out = append(out, driveGrant{Name: sh.Folder(), MappedPath: sh.Mapped, VirtualPath: sh.VPath(), ReadOnly: sh.ReadOnly})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VirtualPath < out[j].VirtualPath })
	return out
}

func anyIn(list, among []string) bool {
	for _, v := range among {
		if contains(list, v) {
			return true
		}
	}
	return false
}
