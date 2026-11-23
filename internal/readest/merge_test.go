package readest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bookroom/internal/store/db"
)

func TestMergeBookFieldClocks(t *testing.T) {
	// The server row is newer (page turns) but the client set a reading status and a shelf later: both
	// survive on the server row without touching its updated_at (#4634, #5911).
	server := &db.Book{BookHash: "h", Title: "T", UpdatedAt: new(int64(2000)), ReadingStatus: new("reading"),
		ReadingStatusUpdatedAt: new(int64(100)), Progress: new("[5,10]")}
	client := &db.Book{BookHash: "h", Title: "T", UpdatedAt: new(int64(1000)), ReadingStatus: new("finished"),
		ReadingStatusUpdatedAt: new(int64(1500)), GroupName: new("小说"), GroupID: new("abc1234"), GroupUpdatedAt: new(int64(1500))}
	out := mergeBook(client, server)
	require.NotNil(t, out)
	assert.Equal(t, "finished", *out.ReadingStatus)
	assert.Equal(t, "小说", *out.GroupName)
	assert.Equal(t, int64(2000), *out.UpdatedAt, "server row keeps its updated_at")
	assert.Equal(t, "[5,10]", *out.Progress, "server row keeps its progress")
}

func TestMergeBookNoOpWhenOnlyClocksDiffer(t *testing.T) {
	server := &db.Book{BookHash: "h", Title: "T", UpdatedAt: new(int64(2000)), ReadingStatus: new("reading"), ReadingStatusUpdatedAt: new(int64(100))}
	client := &db.Book{BookHash: "h", Title: "T", UpdatedAt: new(int64(1000)), ReadingStatus: new("reading"), ReadingStatusUpdatedAt: new(int64(900))}
	assert.Nil(t, mergeBook(client, server))
}

func TestMergeBookMetadataNeverErased(t *testing.T) {
	server := &db.Book{BookHash: "h", Title: "T", UpdatedAt: new(int64(1000)), Metadata: new(`{"description":"d"}`)}
	client := &db.Book{BookHash: "h", Title: "T", UpdatedAt: new(int64(2000))}
	out := mergeBook(client, server)
	require.NotNil(t, out)
	assert.Equal(t, `{"description":"d"}`, *out.Metadata, "#5912: a metadata-less peer must not erase it")
}

func TestMergeBookGroupTieKeepsExistingGroup(t *testing.T) {
	server := &db.Book{BookHash: "h", UpdatedAt: new(int64(1000)), GroupName: new("A"), GroupID: new("x")}
	client := &db.Book{BookHash: "h", UpdatedAt: new(int64(2000))} // unstamped legacy row without a group
	out := mergeBook(client, server)
	require.NotNil(t, out)
	assert.Equal(t, "A", *out.GroupName)
}

func TestMergeBookPageCountNotErased(t *testing.T) {
	server := &db.Book{BookHash: "h", UpdatedAt: new(int64(1000)), PageCount: new(int64(320))}
	client := &db.Book{BookHash: "h", UpdatedAt: new(int64(2000))}
	out := mergeBook(client, server)
	require.NotNil(t, out)
	assert.Equal(t, int64(320), *out.PageCount)
}

func TestMergeConfigKeepsOmittedAndUnionsPacks(t *testing.T) {
	server := &db.BookConfig{BookHash: "h", UpdatedAt: new(int64(1000)), ViewSettings: new(`{"zoom":1}`), TtsPacks: new(`{"readest":["a.mp3"]}`)}
	client := &db.BookConfig{BookHash: "h", UpdatedAt: new(int64(2000)), Xpointer: new("/body/x"), TtsPacks: new(`{"readest":["b.mp3"]}`)}
	out := mergeConfig(client, server, configOmitted{view: true, rsvp: true, search: true})
	require.NotNil(t, out)
	assert.Equal(t, `{"zoom":1}`, *out.ViewSettings)
	assert.JSONEq(t, `{"readest":["a.mp3","b.mp3"]}`, *out.TtsPacks)

	// Server wins the row, yet the client's new pack is grafted on.
	older := &db.BookConfig{BookHash: "h", UpdatedAt: new(int64(500)), TtsPacks: new(`{"webdav":["c.mp3"]}`)}
	out = mergeConfig(older, server, configOmitted{})
	require.NotNil(t, out)
	assert.Equal(t, int64(1000), *out.UpdatedAt)
	assert.JSONEq(t, `{"readest":["a.mp3"],"webdav":["c.mp3"]}`, *out.TtsPacks)

	assert.Nil(t, mergeConfig(&db.BookConfig{BookHash: "h", UpdatedAt: new(int64(500))}, server, configOmitted{}))
}

func TestMergeNoteKeepsOmittedColumns(t *testing.T) {
	server := &db.BookNote{ID: "n", Text: new("x"), UpdatedAt: new(int64(1000)), Global: new(int64(1)), Ink: new(`{"s":[]}`), Crosspoint: new(`{"p":1}`)}
	client := &db.BookNote{ID: "n", Text: new("x"), UpdatedAt: new(int64(2000))}
	out := mergeNote(client, server, noteOmitted{global: true, ink: true, crosspoint: true})
	require.NotNil(t, out)
	assert.Equal(t, int64(1), *out.Global)
	assert.Equal(t, `{"s":[]}`, *out.Ink)
	assert.Equal(t, `{"p":1}`, *out.Crosspoint)

	client.Text = new("changed")
	out = mergeNote(client, server, noteOmitted{crosspoint: true})
	require.NotNil(t, out)
	assert.Nil(t, out.Crosspoint, "new text drops the old anchor")
}

func TestDedupeNotesPrefersTombstoneOnTie(t *testing.T) {
	notes := []db.BookNote{
		{BookHash: "a", ID: "n", UpdatedAt: new(int64(1000))},
		{BookHash: "b", ID: "n", UpdatedAt: new(int64(500)), DeletedAt: new(int64(1000))},
		{BookHash: "a", ID: "m", UpdatedAt: new(int64(1))},
	}
	out := dedupeNotes(notes, func(n *db.BookNote) string { return n.ID })
	require.Len(t, out, 2)
	assert.Equal(t, "b", out[0].BookHash)
	assert.Equal(t, "m", out[1].ID)
}

func TestMergeReplicaFieldsByHLC(t *testing.T) {
	stored := &db.Replica{FieldsJsonb: `{"name":{"v":"a","t":"0000000000002-00000000-d1","s":"d1"},"size":{"v":1,"t":"0000000000005-00000000-d1","s":"d1"}}`,
		UpdatedAtTs: "0000000000005-00000000-d1", SchemaVersion: 1}
	incoming := &db.Replica{FieldsJsonb: `{"name":{"v":"b","t":"0000000000003-00000000-d2","s":"d2"},"size":{"v":2,"t":"0000000000004-00000000-d2","s":"d2"}}`,
		UpdatedAtTs: "0000000000003-00000000-d2", SchemaVersion: 1}
	out := mergeReplicaRows(stored, incoming, 99)
	assert.JSONEq(t, `{"name":{"v":"b","t":"0000000000003-00000000-d2","s":"d2"},"size":{"v":1,"t":"0000000000005-00000000-d1","s":"d1"}}`, out.FieldsJsonb)
	assert.Equal(t, "0000000000005-00000000-d1", out.UpdatedAtTs)

	// A deletion newer than the reincarnation clears it.
	deleted := &db.Replica{FieldsJsonb: `{}`, UpdatedAtTs: "0000000000006-00000000-d2", DeletedAtTs: new("0000000000006-00000000-d2"), SchemaVersion: 1}
	stored.Reincarnation = new("r1")
	out = mergeReplicaRows(stored, deleted, 99)
	assert.Nil(t, out.Reincarnation)
	assert.Equal(t, "0000000000006-00000000-d2", out.UpdatedAtTs)
}

func TestSafeObjectKeyName(t *testing.T) {
	assert.True(t, SafeObjectKeyName("Readest/Books/abc/书.epub"))
	for _, bad := range []string{"", "/a", "a//b", "a/../b", "a/%2e%2e/b", `a\b`, "a/./b"} {
		assert.False(t, SafeObjectKeyName(bad), bad)
	}
}
