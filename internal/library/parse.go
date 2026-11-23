package library

import (
	"bytes"
	"context"
	"html"
	"path"
	"regexp"
	"strings"
)

// maxCoverBytes bounds embedded covers read into memory.
const maxCoverBytes = 10 << 20

// Parse reads the embedded metadata of a book file. It never fails: unreadable files fall back to what the
// file name says, because a damaged or exotic file is still a book the user wants in the library.
func Parse(ctx context.Context, filePath string, format Format, originalName string) Metadata {
	var m Metadata
	var err error
	switch format {
	case EPUB:
		m, err = parseEPUB(filePath)
	case PDF:
		m, err = parsePDF(ctx, filePath)
	case CBZ:
		m, err = parseCBZ(filePath)
	case CBR:
		m, err = parseCBR(filePath)
	}
	_ = err // a parse failure only means fewer fields; the file-name fallback below still applies
	if m.Title == "" || len(m.Authors) == 0 {
		title, author := FilenameMetadata(originalName)
		if m.Title == "" {
			m.Title = title
		}
		if len(m.Authors) == 0 && author != "" {
			m.Authors = []string{author}
		}
	}
	m.Authors = cleanList(m.Authors)
	m.Tags = cleanList(m.Tags)
	m.Description = strings.TrimSpace(m.Description)
	if isbn, ok := NormalizeISBN(m.ISBN); ok {
		m.ISBN = isbn
	} else {
		m.ISBN = ""
	}
	return m
}

var (
	tagRe       = regexp.MustCompile(`(?s)<[^>]*>`)
	blockTagRe  = regexp.MustCompile(`(?i)<\s*(br|/p|/div|/li|/h[1-6])\s*/?>`)
	blankLineRe = regexp.MustCompile(`\n{3,}`)
)

// stripHTML turns an HTML fragment (EPUB descriptions, Google Books) into plain text with paragraph breaks.
func stripHTML(s string) string {
	if !strings.Contains(s, "<") {
		return strings.TrimSpace(html.UnescapeString(s))
	}
	s = blockTagRe.ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return strings.TrimSpace(blankLineRe.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

// imageExt maps an image file name or media type to a cover extension; "" when not an image.
func imageExt(nameOrType string) string {
	s := strings.ToLower(nameOrType)
	switch {
	case strings.HasSuffix(s, ".jpg"), strings.HasSuffix(s, ".jpeg"), s == "image/jpeg", s == "image/jpg":
		return "jpg"
	case strings.HasSuffix(s, ".png"), s == "image/png":
		return "png"
	case strings.HasSuffix(s, ".gif"), s == "image/gif":
		return "gif"
	case strings.HasSuffix(s, ".webp"), s == "image/webp":
		return "webp"
	}
	return ""
}

// sniffImage detects an image type from its first bytes.
func sniffImage(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}):
		return "jpg"
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "png"
	case bytes.HasPrefix(b, []byte("GIF8")):
		return "gif"
	case len(b) > 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "webp"
	}
	return ""
}

// naturalLess orders comic page names so that "page2" precedes "page10".
func naturalLess(a, b string) bool {
	a, b = strings.ToLower(path.Clean(a)), strings.ToLower(path.Clean(b))
	for a != "" && b != "" {
		da, db := digitPrefix(a), digitPrefix(b)
		if da != "" && db != "" {
			na, nb := strings.TrimLeft(da, "0"), strings.TrimLeft(db, "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			a, b = a[len(da):], b[len(db):]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func digitPrefix(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}
