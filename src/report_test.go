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

package main

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestReportIsAPDFWithoutSecrets(t *testing.T) {
	_, h := newUIServer(t, []map[string]any{
		{"username": "alice", "status": 1, "password": "hunter2", "home_dir": "/srv/data/alice",
			"public_keys": []any{"ssh-ed25519 AAAAkeymaterial"}, "filesystem": map[string]any{"provider": 0}},
	})
	if rec := get(h, "/mlw/report", nil); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/mlw/login" {
		t.Fatalf("anonymous: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	rec := get(h, "/mlw/report", login(t, h))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/pdf" ||
		!strings.Contains(rec.Header().Get("Content-Disposition"), "attachment; filename=\"mlw-storage-report-") {
		t.Fatalf("report: %d %v", rec.Code, rec.Header())
	}
	b := rec.Body.Bytes()
	if !bytes.HasPrefix(b, []byte("%PDF-1.4")) || !bytes.HasSuffix(b, []byte("%%EOF\n")) {
		t.Fatal("not a PDF")
	}
	for _, want := range []string{`Users \(1\)`, `alice \(Active\)`, "home_dir: /srv/data/alice", `Groups \(0\)`, `Virtual folders \(0\)`} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("report lacks %q", want)
		}
	}
	for _, secret := range []string{"hunter2", "keymaterial"} {
		if bytes.Contains(b, []byte(secret)) {
			t.Errorf("report leaks %q", secret)
		}
	}
}

func TestPDFPaginatesAndEscapes(t *testing.T) {
	d := newPDF("t", "foot (x)")
	for i := 0; i < 300; i++ {
		d.text(0, "line (with) parens \\ and ünï "+strings.Repeat("x", 200))
	}
	b := d.bytes()
	if len(d.pages) < 5 || !bytes.Contains(b, []byte("\\(with\\)")) || !bytes.Contains(b, []byte("/Count ")) {
		t.Fatalf("pages=%d", len(d.pages))
	}
	if !bytes.Contains(b, []byte("startxref")) {
		t.Fatal("no xref")
	}
}
