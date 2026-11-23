package library

import (
	"encoding/json"
	"slices"
	"strings"

	"bookroom/internal/api"
	"bookroom/internal/store/db"
)

// Metadata is a set of descriptive fields from any source: the file itself, a user edit or a scrape.
type Metadata struct {
	Title       string
	Authors     []string
	Series      string
	SeriesIndex *float64
	Description string
	Publisher   string
	Published   string
	Language    string
	ISBN        string
	Tags        []string
	PageCount   *int
	WorkType    api.WorkType
	Webnovel    *api.WebnovelInfo
	Cover       []byte // image bytes, when the source provides one
	CoverExt    string // jpg, png, gif or webp
}

// AllFields lists every metadata field in display order.
var AllFields = []api.MetadataField{
	api.MetadataFieldTitle, api.MetadataFieldAuthors, api.MetadataFieldSeries, api.MetadataFieldSeriesIndex, api.MetadataFieldDescription, api.MetadataFieldPublisher, api.MetadataFieldPublished,
	api.MetadataFieldLanguage, api.MetadataFieldIsbn, api.MetadataFieldTags, api.MetadataFieldCover, api.MetadataFieldWorkType,
}

// has reports whether m carries a value for f.
func (m *Metadata) has(f api.MetadataField) bool {
	switch f {
	case api.MetadataFieldTitle:
		return m.Title != ""
	case api.MetadataFieldAuthors:
		return len(m.Authors) > 0
	case api.MetadataFieldSeries:
		return m.Series != ""
	case api.MetadataFieldSeriesIndex:
		return m.SeriesIndex != nil
	case api.MetadataFieldDescription:
		return m.Description != ""
	case api.MetadataFieldPublisher:
		return m.Publisher != ""
	case api.MetadataFieldPublished:
		return m.Published != ""
	case api.MetadataFieldLanguage:
		return m.Language != ""
	case api.MetadataFieldIsbn:
		return m.ISBN != ""
	case api.MetadataFieldTags:
		return len(m.Tags) > 0
	case api.MetadataFieldCover:
		return len(m.Cover) > 0
	case api.MetadataFieldWorkType:
		return m.WorkType != "" && m.WorkType != api.WorkTypeUnknown
	}
	return false
}

// empty reports whether the library row has no value for f.
func empty(lb *db.LibraryBook, f api.MetadataField) bool {
	switch f {
	case api.MetadataFieldTitle:
		return lb.Title == ""
	case api.MetadataFieldAuthors:
		return lb.Authors == "" || lb.Authors == "[]"
	case api.MetadataFieldSeries:
		return lb.Series == ""
	case api.MetadataFieldSeriesIndex:
		return lb.SeriesIndex == nil
	case api.MetadataFieldDescription:
		return lb.Description == ""
	case api.MetadataFieldPublisher:
		return lb.Publisher == ""
	case api.MetadataFieldPublished:
		return lb.Published == ""
	case api.MetadataFieldLanguage:
		return lb.Language == ""
	case api.MetadataFieldIsbn:
		return lb.Isbn == ""
	case api.MetadataFieldTags:
		return lb.Tags == "" || lb.Tags == "[]"
	case api.MetadataFieldCover:
		return lb.CoverFile == ""
	case api.MetadataFieldWorkType:
		return lb.WorkType == "" || lb.WorkType == string(api.WorkTypeUnknown)
	}
	return false
}

// set copies field f from m into the row (cover bytes are written by the caller).
func set(lb *db.LibraryBook, m *Metadata, f api.MetadataField) {
	switch f {
	case api.MetadataFieldTitle:
		lb.Title = m.Title
	case api.MetadataFieldAuthors:
		lb.Authors = encodeList(m.Authors)
	case api.MetadataFieldSeries:
		lb.Series = m.Series
	case api.MetadataFieldSeriesIndex:
		lb.SeriesIndex = m.SeriesIndex
	case api.MetadataFieldDescription:
		lb.Description = m.Description
	case api.MetadataFieldPublisher:
		lb.Publisher = m.Publisher
	case api.MetadataFieldPublished:
		lb.Published = m.Published
	case api.MetadataFieldLanguage:
		lb.Language = m.Language
	case api.MetadataFieldIsbn:
		lb.Isbn = m.ISBN
	case api.MetadataFieldTags:
		lb.Tags = encodeList(m.Tags)
	case api.MetadataFieldWorkType:
		lb.WorkType = string(m.WorkType)
		if m.WorkType == api.WorkTypeWebnovel && m.Webnovel != nil {
			b, _ := json.Marshal(m.Webnovel)
			lb.Webnovel = new(string(b))
		}
	}
}

// cleanList trims, drops empties and de-duplicates (case-insensitively) while keeping order.
func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		k := strings.ToLower(s)
		if s == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s)
	}
	return out
}

func encodeList(in []string) string {
	b, _ := json.Marshal(cleanList(in))
	return string(b)
}

func decodeList(s string) []string {
	var out []string
	if s == "" || json.Unmarshal([]byte(s), &out) != nil || out == nil {
		return []string{}
	}
	return out
}

func decodeFields(s string) []api.MetadataField {
	var out []api.MetadataField
	if json.Unmarshal([]byte(s), &out) != nil || out == nil {
		return []api.MetadataField{}
	}
	return out
}

func encodeFields(fs []api.MetadataField) string {
	slices.Sort(fs)
	fs = slices.Compact(fs)
	b, _ := json.Marshal(fs)
	return string(b)
}

// ValidField reports whether f is a known metadata field.
func ValidField(f api.MetadataField) bool { return slices.Contains(AllFields, f) }
