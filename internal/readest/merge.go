// Package readest implements the Readest cloud contract (/api/sync, /api/sync/replicas, /api/storage) on top
// of the BookRoom store. Merge rules are ported from Readest v0.12.20 `pages/api/sync.ts`; the function
// names below match the TypeScript ones so the two can be compared side by side.
package readest

import (
	"encoding/json"
	"slices"

	"bookroom/internal/store/db"
)

func ms(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func str(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// nullableEq compares two nullable strings treating nil and nil as equal (JavaScript `(a ?? null) !== (b ?? null)`).
func nullableEq(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// readingStatusChanged: undefined and null both mean "no status".
func readingStatusChanged(client, server *string) bool { return !nullableEq(client, server) }

// resolveReadingStatusMerge: newer reading_status_updated_at wins, ties go to the client.
func resolveReadingStatusMerge(client, server *db.Book) (*string, *int64) {
	if ms(client.ReadingStatusUpdatedAt) >= ms(server.ReadingStatusUpdatedAt) {
		return client.ReadingStatus, client.ReadingStatusUpdatedAt
	}
	return server.ReadingStatus, server.ReadingStatusUpdatedAt
}

// resolveCoverMerge: newer cover_updated_at wins, ties go to the client.
func resolveCoverMerge(client, server *db.Book) (*string, *int64) {
	if ms(client.CoverUpdatedAt) >= ms(server.CoverUpdatedAt) {
		return client.CoverHash, client.CoverUpdatedAt
	}
	return server.CoverHash, server.CoverUpdatedAt
}

// resolvePageCountMerge: the row winner's count, else the other side's; a missing count never erases one.
func resolvePageCountMerge(client, server *db.Book, clientIsNewer bool) *int64 {
	winner, loser := server, client
	if clientIsNewer {
		winner, loser = client, server
	}
	if winner.PageCount != nil {
		return winner.PageCount
	}
	return loser.PageCount
}

type metadataFields struct {
	title, author     string
	tags              *string
	metadata          *string
	metadataUpdatedAt *int64
}

func pickMetadata(b *db.Book) metadataFields {
	return metadataFields{b.Title, b.Author, b.Tags, b.Metadata, b.MetadataUpdatedAt}
}

// resolveMetadataMerge: the metadata group resolves on metadata_updated_at; a tie follows the row winner. An
// absent metadata blob never overwrites one (#5912).
func resolveMetadataMerge(client, server *db.Book, clientRowWins bool) metadataFields {
	c, s := ms(client.MetadataUpdatedAt), ms(server.MetadataUpdatedAt)
	clientWins := c > s
	if c == s {
		clientWins = clientRowWins
	}
	winner, loser := server, client
	if clientWins {
		winner, loser = client, server
	}
	f := pickMetadata(winner)
	if f.metadata == nil {
		f.metadata = loser.Metadata
	}
	return f
}

// bookMetadataChanged compares values only (tags as their JSON array).
func bookMetadataChanged(a metadataFields, b *db.Book) bool {
	return a.title != b.Title || a.author != b.Author || !nullableEq(a.metadata, b.Metadata) || !jsonEqual(a.tags, b.Tags)
}

type groupFields struct {
	id, name  *string
	updatedAt *int64
}

func hasGroup(b *db.Book) bool { return str(b.GroupID) != "" || str(b.GroupName) != "" }

// resolveGroupMerge: group_updated_at decides; on a tie the side that has a group wins (an absent group is
// ambiguous), and only when both agree does the row winner decide (#5911).
func resolveGroupMerge(client, server *db.Book, clientRowWins bool) groupFields {
	pick := func(b *db.Book) groupFields { return groupFields{b.GroupID, b.GroupName, b.GroupUpdatedAt} }
	c, s := ms(client.GroupUpdatedAt), ms(server.GroupUpdatedAt)
	if c != s {
		if c > s {
			return pick(client)
		}
		return pick(server)
	}
	if hasGroup(client) != hasGroup(server) {
		if hasGroup(client) {
			return pick(client)
		}
		return pick(server)
	}
	if clientRowWins {
		return pick(client)
	}
	return pick(server)
}

func bookGroupChanged(a groupFields, b *db.Book) bool {
	return !nullableEq(a.id, b.GroupID) || !nullableEq(a.name, b.GroupName)
}

// mergeBook resolves a pushed books row against the stored one. It returns the row to write, or nil when the
// stored row stays authoritative.
func mergeBook(client, server *db.Book) *db.Book {
	clientIsNewer := ms(client.DeletedAt) > ms(server.DeletedAt) || ms(client.UpdatedAt) > ms(server.UpdatedAt)
	status, statusAt := resolveReadingStatusMerge(client, server)
	cover, coverAt := resolveCoverMerge(client, server)
	meta := resolveMetadataMerge(client, server, clientIsNewer)
	group := resolveGroupMerge(client, server, clientIsNewer)
	pageCount := resolvePageCountMerge(client, server, clientIsNewer)
	// Import provenance only fills a blank.
	originalFilename := server.OriginalFilename
	if str(originalFilename) == "" {
		originalFilename = client.OriginalFilename
	}
	graft := func(b *db.Book) *db.Book {
		out := *b
		out.ReadingStatus, out.ReadingStatusUpdatedAt = status, statusAt
		out.CoverHash, out.CoverUpdatedAt = cover, coverAt
		out.Title, out.Author, out.Tags, out.Metadata, out.MetadataUpdatedAt = meta.title, meta.author, meta.tags, meta.metadata, meta.metadataUpdatedAt
		out.GroupID, out.GroupName, out.GroupUpdatedAt = group.id, group.name, group.updatedAt
		out.PageCount = pageCount
		out.OriginalFilename = originalFilename
		return &out
	}
	if clientIsNewer {
		return graft(client)
	}
	changed := readingStatusChanged(status, server.ReadingStatus) ||
		!nullableEq(cover, server.CoverHash) ||
		bookMetadataChanged(meta, server) ||
		bookGroupChanged(group, server) ||
		!int64PtrEq(pageCount, server.PageCount) ||
		!nullableEq(originalFilename, server.OriginalFilename)
	if !changed {
		return nil
	}
	// Server wins the row but the client holds fresher fields: graft them, keep updated_at so the date-read
	// order does not jump; the new synced_at makes peers re-pull.
	return graft(server)
}

func int64PtrEq(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// configOmitted marks BookConfig members the client left out (as opposed to sending null).
type configOmitted struct{ rsvp, search, view bool }

// keepOmittedConfigColumns keeps stored values of columns the push left out; position columns are not kept
// because a pull takes the further of location and xpointer.
func keepOmittedConfigColumns(client, server *db.BookConfig, om configOmitted) *db.BookConfig {
	out := *client
	if om.rsvp {
		out.RsvpPosition = server.RsvpPosition
	}
	if om.search {
		out.SearchConfig = server.SearchConfig
	}
	if om.view {
		out.ViewSettings = server.ViewSettings
	}
	return &out
}

// mergeTTSPacks unions read-aloud pack names per destination (utils/ttsPacks.ts). The result is
// canonical JSON (sorted destinations), so two unions compare by text.
func mergeTTSPacks(a, b *string) *string {
	if a == nil && b == nil {
		return nil
	}
	merged := map[string][]string{}
	for _, side := range []*string{a, b} {
		if side == nil {
			continue
		}
		var packs map[string]json.RawMessage
		if json.Unmarshal([]byte(*side), &packs) != nil {
			continue
		}
		for dest, raw := range packs {
			var names []any
			if json.Unmarshal(raw, &names) != nil {
				continue
			}
			list := merged[dest]
			if list == nil {
				list = []string{}
			}
			for _, n := range names {
				if s, ok := n.(string); ok && !slices.Contains(list, s) {
					list = append(list, s)
				}
			}
			merged[dest] = list
		}
	}
	out, _ := json.Marshal(merged)
	return new(string(out))
}

// resolveTTSPacksMerge returns the union and whether it differs from the stored packs.
func resolveTTSPacksMerge(client, server *string) (*string, bool) {
	var serverPacks *string
	if server != nil && *server != "" {
		serverPacks = server
	}
	var clientPacks *string
	if client != nil && *client != "" {
		clientPacks = client
	}
	merged := mergeTTSPacks(serverPacks, clientPacks)
	if merged == nil {
		return nil, false
	}
	var canonicalServer *string
	if serverPacks != nil {
		canonicalServer = mergeTTSPacks(serverPacks, nil)
	}
	return merged, !nullableEq(merged, canonicalServer)
}

// mergeConfig resolves a pushed book config. packOnly marks a pack-only seed (updatedAt 0 with packs).
func mergeConfig(client, server *db.BookConfig, om configOmitted) *db.BookConfig {
	clientIsNewer := ms(client.DeletedAt) > ms(server.DeletedAt) || ms(client.UpdatedAt) > ms(server.UpdatedAt)
	packs, packsChanged := resolveTTSPacksMerge(client.TtsPacks, server.TtsPacks)
	if clientIsNewer {
		out := keepOmittedConfigColumns(client, server, om)
		if packs != nil {
			out.TtsPacks = packs
		}
		out.CreatedAt = server.CreatedAt
		return out
	}
	if packsChanged {
		out := *server
		out.TtsPacks = packs
		return &out
	}
	return nil
}

// noteOmitted marks BookNote members the client left out.
type noteOmitted struct{ global, ink, crosspoint bool }

// keepOmittedNoteColumns keeps stored global/ink/crosspoint a push left out; new text drops the old anchor.
func keepOmittedNoteColumns(client, server *db.BookNote, om noteOmitted) *db.BookNote {
	out := *client
	if om.global {
		out.Global = server.Global
	}
	if om.ink {
		out.Ink = server.Ink
	}
	if om.crosspoint {
		out.Crosspoint = nil
		if nullableEq(client.Text, server.Text) {
			out.Crosspoint = server.Crosspoint
		}
	}
	return &out
}

// takesNoteAnchor: a losing note may still give a row without one its CrossPoint anchor.
func takesNoteAnchor(client, server *db.BookNote) bool {
	return str(client.Crosspoint) != "" && str(server.Crosspoint) == "" && server.DeletedAt == nil &&
		nullableEq(client.Text, server.Text) && (str(server.Cfi) == "" || str(client.Cfi) == str(server.Cfi))
}

// mergeNote resolves a pushed note against the stored one.
func mergeNote(client, server *db.BookNote, om noteOmitted) *db.BookNote {
	clientIsNewer := ms(client.DeletedAt) > ms(server.DeletedAt) || ms(client.UpdatedAt) > ms(server.UpdatedAt)
	if clientIsNewer {
		return keepOmittedNoteColumns(client, server, om)
	}
	if takesNoteAnchor(client, server) {
		out := *server
		out.Crosspoint = client.Crosspoint
		return &out
	}
	return nil
}

// latestChangeMs counts a delete as a change (#5818).
func latestChangeMs(updated, deleted *int64) int64 { return max(ms(updated), ms(deleted)) }

// outranksCopy: whether a note beats the row another copy of the book holds for it; ties go to tombstones.
func outranksCopy(client, copyRow *db.BookNote) bool {
	at, copyAt := latestChangeMs(client.UpdatedAt, client.DeletedAt), latestChangeMs(copyRow.UpdatedAt, copyRow.DeletedAt)
	return at > copyAt || (at == copyAt && client.DeletedAt != nil && copyRow.DeletedAt == nil)
}

// dedupeNotes collapses notes sharing key to the most recently changed one, keeping input order; a tie goes
// to the tombstone.
func dedupeNotes(notes []db.BookNote, key func(*db.BookNote) string) []db.BookNote {
	type best struct {
		idx int
		at  int64
	}
	latest := map[string]best{}
	for i := range notes {
		k := key(&notes[i])
		if k == "" {
			continue
		}
		at := latestChangeMs(notes[i].UpdatedAt, notes[i].DeletedAt)
		b, ok := latest[k]
		if !ok || at > b.at || (at == b.at && notes[i].DeletedAt != nil && notes[b.idx].DeletedAt == nil) {
			latest[k] = best{i, at}
		}
	}
	out := make([]db.BookNote, 0, len(notes))
	for i := range notes {
		k := key(&notes[i])
		if k == "" || latest[k].idx == i {
			out = append(out, notes[i])
		}
	}
	return out
}

// jsonEqual compares two JSON texts structurally (JSON.stringify of the parsed value), nil equal to "null".
func jsonEqual(a, b *string) bool {
	norm := func(s *string) string {
		if s == nil {
			return "null"
		}
		var v any
		if json.Unmarshal([]byte(*s), &v) != nil {
			return *s
		}
		out, _ := json.Marshal(v)
		return string(out)
	}
	return norm(a) == norm(b)
}
