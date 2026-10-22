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

// A minimal PDF writer: A4 pages of text in the standard Helvetica and
// Courier fonts, with automatic line wrapping and page breaks. It exists so
// the configuration report needs no third-party dependency.

import (
	"bytes"
	"fmt"
	"strings"
)

const (
	pdfW, pdfH   = 595, 842
	pdfMargin    = 50
	pdfBodySize  = 8.5
	pdfBodyChars = 96 // (page width - margins) / (body size * Courier glyph width of 0.6)
)

type pdfDoc struct {
	title  string
	pages  []*bytes.Buffer
	y      float64
	footer string
}

func newPDF(title, footer string) *pdfDoc {
	d := &pdfDoc{title: title, footer: footer}
	d.newPage()
	return d
}

func (d *pdfDoc) newPage() {
	d.pages = append(d.pages, &bytes.Buffer{})
	d.y = pdfH - pdfMargin
}

// pdfText converts s to the single-byte encoding the standard fonts use and
// escapes the characters that are special in a PDF string.
func pdfText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\\' || r == '(' || r == ')':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString("    ")
		case r >= 0x20 && r < 0x7f:
			b.WriteRune(r)
		case r >= 0xa0 && r <= 0xff:
			fmt.Fprintf(&b, "\\%03o", r)
		default:
			b.WriteByte('?')
		}
	}
	return b.String()
}

func (d *pdfDoc) line(font string, size float64, x float64, s string) {
	lead := size * 1.35
	if d.y-lead < pdfMargin+10 {
		d.newPage()
	}
	d.y -= lead
	fmt.Fprintf(d.pages[len(d.pages)-1], "BT /%s %.1f Tf %.1f %.1f Td (%s) Tj ET\n", font, size, x, d.y, pdfText(s))
}

func (d *pdfDoc) gap(h float64) { d.y -= h }

// heading starts a section: a bold title with a rule beneath it.
func (d *pdfDoc) heading(s string, size float64) {
	if d.y < pdfMargin+60 {
		d.newPage()
	}
	d.gap(size * 0.6)
	d.line("F2", size, pdfMargin, s)
	fmt.Fprintf(d.pages[len(d.pages)-1], "0.6 w %.1f %.1f m %.1f %.1f l S\n", float64(pdfMargin), d.y-3, float64(pdfW-pdfMargin), d.y-3)
	d.gap(4)
}

func (d *pdfDoc) subheading(s string) {
	if d.y < pdfMargin+40 {
		d.newPage()
	}
	d.gap(4)
	d.line("F2", 10, pdfMargin, s)
}

// text writes monospaced lines, wrapping long ones with a hanging indent.
func (d *pdfDoc) text(indent int, s string) {
	pad := strings.Repeat(" ", indent)
	width := pdfBodyChars - indent
	for {
		r := []rune(s)
		if len(r) <= width {
			d.line("F3", pdfBodySize, pdfMargin, pad+s)
			return
		}
		cut := width
		if i := strings.LastIndexAny(string(r[:width]), " ,/"); i > width/2 {
			cut = len([]rune(string(r[:width])[:i+1]))
		}
		d.line("F3", pdfBodySize, pdfMargin, pad+string(r[:cut]))
		s = "  " + strings.TrimLeft(string(r[cut:]), " ")
	}
}

// bytes assembles the finished file.
func (d *pdfDoc) bytes() []byte {
	var out bytes.Buffer
	var offs []int
	obj := func(body string) {
		offs = append(offs, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", len(offs), body)
	}
	n := len(d.pages)
	out.WriteString("%PDF-1.4\n")
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	kids := make([]string, n)
	for i := range kids {
		kids[i] = fmt.Sprintf("%d 0 R", 7+2*i)
	}
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n))
	for _, f := range []string{"Helvetica", "Helvetica-Bold", "Courier"} {
		obj(fmt.Sprintf("<< /Type /Font /Subtype /Type1 /BaseFont /%s /Encoding /WinAnsiEncoding >>", f))
	}
	obj(fmt.Sprintf("<< /Title (%s) /Producer (mlw-storage) >>", pdfText(d.title)))
	for i, p := range d.pages {
		var c bytes.Buffer
		c.Write(p.Bytes())
		fmt.Fprintf(&c, "BT /F1 8 Tf %d 30 Td (%s) Tj ET\n", pdfMargin, pdfText(d.footer))
		fmt.Fprintf(&c, "BT /F1 8 Tf %d 30 Td (Page %d of %d) Tj ET\n", pdfW-pdfMargin-48, i+1, n)
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %d %d] /Contents %d 0 R /Resources << /Font << /F1 3 0 R /F2 4 0 R /F3 5 0 R >> >> >>",
			pdfW, pdfH, 8+2*i))
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", c.Len(), c.String()))
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(offs)+1)
	for _, o := range offs {
		fmt.Fprintf(&out, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R /Info 6 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offs)+1, xref)
	return out.Bytes()
}
