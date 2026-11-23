package library

import (
	"path/filepath"
	"strings"
)

// Format is a book format as Readest names it (`EXTS` in libs/document.ts) plus CBR.
type Format string

const (
	EPUB Format = "EPUB"
	PDF  Format = "PDF"
	MOBI Format = "MOBI"
	AZW  Format = "AZW"
	AZW3 Format = "AZW3"
	CBZ  Format = "CBZ"
	CBR  Format = "CBR"
	FB2  Format = "FB2"
	FBZ  Format = "FBZ"
	TXT  Format = "TXT"
	MD   Format = "MD"
)

var formatByExt = map[string]Format{
	"epub": EPUB, "pdf": PDF, "mobi": MOBI, "azw": AZW, "azw3": AZW3, "cbz": CBZ, "cbr": CBR,
	"fb2": FB2, "fbz": FBZ, "txt": TXT, "md": MD,
}

var mimeByFormat = map[Format]string{
	EPUB: "application/epub+zip",
	PDF:  "application/pdf",
	MOBI: "application/x-mobipocket-ebook",
	AZW:  "application/vnd.amazon.ebook",
	AZW3: "application/vnd.amazon.mobi8-ebook",
	CBZ:  "application/vnd.comicbook+zip",
	CBR:  "application/vnd.comicbook-rar",
	FB2:  "application/x-fictionbook+xml",
	FBZ:  "application/x-zip-compressed-fb2",
	TXT:  "text/plain; charset=utf-8",
	MD:   "text/markdown; charset=utf-8",
}

// FormatOf derives the format from a file name; ok is false for unsupported extensions.
func FormatOf(name string) (Format, bool) {
	f, ok := formatByExt[strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")]
	return f, ok
}

// Ext is the lowercase file extension of a format.
func (f Format) Ext() string { return strings.ToLower(string(f)) }

// MIME is the media type served for a format.
func (f Format) MIME() string {
	if m, ok := mimeByFormat[f]; ok {
		return m
	}
	return "application/octet-stream"
}
