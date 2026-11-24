package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"bookroom/internal/library"
	"bookroom/internal/store"
	"bookroom/internal/store/db"
	"bookroom/internal/testutil"
)

func TestImportAndUserCommands(t *testing.T) {
	data := t.TempDir()
	t.Setenv("BOOKROOM_DATA_DIR", data)
	t.Setenv("BOOKROOM_SCRAPE_AUTO", "false")
	t.Setenv("BOOKROOM_PASSWORD", "readerpass1")
	require.NoError(t, run([]string{"user", "add", "-admin", "reader"}))
	require.Error(t, run([]string{"user", "add", "reader"}), "duplicate name")

	inbox := t.TempDir()
	testutil.WriteEPUB(t, inbox, "a.epub", testutil.EPUB{Title: "A", Author: "X"})
	require.NoError(t, os.MkdirAll(filepath.Join(inbox, "sub"), 0o755))
	testutil.WriteFile(t, filepath.Join(inbox, "sub"), "《书》作者.txt", []byte("正文"))
	testutil.WriteFile(t, inbox, "notes.docx", []byte("ignored"))
	testutil.WriteFile(t, inbox, ".hidden.epub", []byte("ignored"))
	require.NoError(t, run([]string{"import", "-user", "reader", "-shelf", "收件箱", inbox}))
	require.NoError(t, run([]string{"import", inbox}), "a second import only finds existing books")
	require.Error(t, run([]string{"import", "-shelf", "x", inbox}), "-shelf needs -user")

	st, err := store.Open(context.Background(), filepath.Join(data, "bookroom.db"))
	require.NoError(t, err)
	defer st.Close()
	books, err := st.Q.ListLibraryBooks(context.Background())
	require.NoError(t, err)
	require.Len(t, books, 2)
	u, err := st.Q.GetUserByLogin(context.Background(), "reader")
	require.NoError(t, err)
	for _, b := range books {
		row, err := st.Q.GetBook(context.Background(), db.GetBookParams{UserID: u.ID, BookHash: b.Hash})
		require.NoError(t, err)
		assert.Equal(t, "收件箱", *row.GroupName)
		assert.Equal(t, library.GroupID("收件箱"), *row.GroupID)
	}

	require.NoError(t, run([]string{"user", "passwd", "reader"}))
	require.NoError(t, run([]string{"user", "list"}))
	require.NoError(t, run([]string{"version"}))
	snap := filepath.Join(t.TempDir(), "snap.db")
	require.NoError(t, run([]string{"backup", snap}))
	copyStore, err := store.Open(context.Background(), snap)
	require.NoError(t, err)
	n, err := copyStore.Q.CountUsers(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	copyStore.Close()
	require.Error(t, run([]string{"backup", snap}), "an existing target is refused")
	require.Error(t, run([]string{"bogus"}))
}
