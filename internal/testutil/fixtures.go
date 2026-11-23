// Package testutil builds small, valid book files for tests.
package testutil

import (
	"archive/zip"
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// EPUB describes a generated EPUB.
type EPUB struct {
	Title, Author, ISBN, Series, SeriesIndex, Language, Description string
	Subjects                                                       []string
	Cover                                                          bool
	Salt                                                           string // changes the bytes (and hash) without changing metadata
}

// PNG returns a tiny valid PNG.
func PNG(t testing.TB) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 3))
	img.Set(0, 0, color.RGBA{200, 10, 10, 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// WriteEPUB writes the EPUB to dir/name and returns its path.
func WriteEPUB(t testing.TB, dir, name string, e EPUB) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add("mimetype", "application/epub+zip")
	add("META-INF/container.xml", `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`)
	meta := fmt.Sprintf(`<dc:title>%s</dc:title><dc:creator opf:role="aut">%s</dc:creator><dc:language>%s</dc:language>`,
		e.Title, e.Author, e.Language)
	if e.ISBN != "" {
		meta += fmt.Sprintf(`<dc:identifier opf:scheme="ISBN">%s</dc:identifier>`, e.ISBN)
	}
	if e.Description != "" {
		meta += fmt.Sprintf(`<dc:description>%s</dc:description>`, e.Description)
	}
	for _, s := range e.Subjects {
		meta += fmt.Sprintf(`<dc:subject>%s</dc:subject>`, s)
	}
	if e.Series != "" {
		meta += fmt.Sprintf(`<meta name="calibre:series" content="%s"/><meta name="calibre:series_index" content="%s"/>`, e.Series, e.SeriesIndex)
	}
	manifest := `<item id="c1" href="c1.xhtml" media-type="application/xhtml+xml"/>`
	if e.Cover {
		manifest += `<item id="cover" href="images/cover.png" media-type="image/png" properties="cover-image"/>`
	}
	add("OEBPS/content.opf", fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:opf="http://www.idpf.org/2007/opf" version="3.0">
  <metadata>%s</metadata>
  <manifest>%s</manifest>
  <spine><itemref idref="c1"/></spine>
</package>`, meta, manifest))
	add("OEBPS/c1.xhtml", `<html xmlns="http://www.w3.org/1999/xhtml"><body><p>Hello `+e.Salt+`</p></body></html>`)
	if e.Cover {
		w, err := zw.Create("OEBPS/images/cover.png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(PNG(t)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return writeFile(t, dir, name, buf.Bytes())
}

// WriteCBZ writes a comic archive with the given page names (each a tiny PNG) and optional ComicInfo.xml.
func WriteCBZ(t testing.TB, dir, name string, pages []string, comicInfo string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, p := range pages {
		w, err := zw.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(PNG(t)); err != nil {
			t.Fatal(err)
		}
	}
	if comicInfo != "" {
		w, err := zw.Create("ComicInfo.xml")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(comicInfo)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return writeFile(t, dir, name, buf.Bytes())
}

// WriteFile writes raw bytes to dir/name.
func WriteFile(t testing.TB, dir, name string, b []byte) string {
	t.Helper()
	return writeFile(t, dir, name, b)
}

func writeFile(t testing.TB, dir, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
