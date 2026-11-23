-- name: GetBooksByHashes :many
SELECT * FROM books WHERE user_id = ? AND book_hash IN (sqlc.slice(hashes));

-- name: GetBook :one
SELECT * FROM books WHERE user_id = ? AND book_hash = ?;

-- name: UpsertBook :exec
INSERT INTO books (
  user_id, book_hash, meta_hash, format, title, source_title, original_filename, author, group_id, group_name,
  group_updated_at, tags, progress, reading_status, reading_status_updated_at, cover_hash, cover_updated_at,
  page_count, metadata, metadata_updated_at, created_at, updated_at, deleted_at, uploaded_at, synced_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (user_id, book_hash) DO UPDATE SET
  meta_hash = excluded.meta_hash, format = excluded.format, title = excluded.title,
  source_title = excluded.source_title, original_filename = excluded.original_filename, author = excluded.author,
  group_id = excluded.group_id, group_name = excluded.group_name, group_updated_at = excluded.group_updated_at,
  tags = excluded.tags, progress = excluded.progress, reading_status = excluded.reading_status,
  reading_status_updated_at = excluded.reading_status_updated_at, cover_hash = excluded.cover_hash,
  cover_updated_at = excluded.cover_updated_at, page_count = excluded.page_count, metadata = excluded.metadata,
  metadata_updated_at = excluded.metadata_updated_at, created_at = excluded.created_at,
  updated_at = excluded.updated_at, deleted_at = excluded.deleted_at, uploaded_at = excluded.uploaded_at,
  synced_at = excluded.synced_at;

-- name: InsertBookIfAbsent :exec
INSERT INTO books (
  user_id, book_hash, format, title, original_filename, author, tags, cover_hash, cover_updated_at, page_count,
  metadata, metadata_updated_at, created_at, updated_at, uploaded_at, synced_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (user_id, book_hash) DO NOTHING;

-- name: PullBooks :many
SELECT * FROM books
WHERE user_id = sqlc.arg(user_id) AND synced_at > sqlc.arg(since)
  AND ((sqlc.narg(book) IS NULL AND sqlc.narg(meta) IS NULL) OR book_hash = sqlc.narg(book) OR meta_hash = sqlc.narg(meta))
ORDER BY synced_at DESC;

-- name: PullBooksPage :many
SELECT * FROM books
WHERE user_id = sqlc.arg(user_id) AND synced_at > sqlc.arg(since)
  AND ((sqlc.narg(book) IS NULL AND sqlc.narg(meta) IS NULL) OR book_hash = sqlc.narg(book) OR meta_hash = sqlc.narg(meta))
ORDER BY synced_at ASC
LIMIT sqlc.arg(lim);

-- name: BooksAtSyncedAt :many
SELECT * FROM books
WHERE user_id = sqlc.arg(user_id) AND synced_at = sqlc.arg(synced_at)
  AND ((sqlc.narg(book) IS NULL AND sqlc.narg(meta) IS NULL) OR book_hash = sqlc.narg(book) OR meta_hash = sqlc.narg(meta));

-- name: SetBookProgressIfOlder :exec
UPDATE books SET progress = sqlc.arg(progress), updated_at = sqlc.arg(updated_at), synced_at = sqlc.arg(synced_at)
WHERE user_id = sqlc.arg(user_id) AND book_hash = sqlc.arg(book_hash)
  AND (updated_at IS NULL OR updated_at < sqlc.arg(updated_at));

-- name: PropagateLibraryMetadata :exec
UPDATE books SET title = sqlc.arg(title), author = sqlc.arg(author), tags = sqlc.arg(tags),
  metadata = sqlc.arg(metadata), metadata_updated_at = sqlc.arg(now), cover_hash = sqlc.arg(cover_hash),
  cover_updated_at = sqlc.arg(cover_updated_at), page_count = COALESCE(page_count, sqlc.arg(page_count)),
  synced_at = sqlc.arg(now)
WHERE book_hash = sqlc.arg(book_hash) AND user_id != sqlc.arg(except_user);

-- name: TombstoneLibraryBook :exec
UPDATE books SET deleted_at = sqlc.arg(now), updated_at = sqlc.arg(now), synced_at = sqlc.arg(now)
WHERE book_hash = sqlc.arg(book_hash) AND deleted_at IS NULL;

-- name: SetBookGroup :execrows
UPDATE books SET group_id = sqlc.arg(group_id), group_name = sqlc.arg(group_name), group_updated_at = sqlc.arg(now),
  synced_at = sqlc.arg(now)
WHERE user_id = sqlc.arg(user_id) AND book_hash = sqlc.arg(book_hash);

-- name: SetBookReadingStatus :execrows
UPDATE books SET reading_status = sqlc.arg(reading_status), reading_status_updated_at = sqlc.arg(now),
  synced_at = sqlc.arg(now)
WHERE user_id = sqlc.arg(user_id) AND book_hash = sqlc.arg(book_hash);

-- name: BooksInShelfTree :many
SELECT book_hash, group_name FROM books
WHERE user_id = sqlc.arg(user_id) AND deleted_at IS NULL
  AND (group_name = sqlc.arg(path) OR substr(group_name, 1, length(sqlc.arg(prefix))) = sqlc.arg(prefix));

-- name: GetConfigsByHashes :many
SELECT * FROM book_configs WHERE user_id = ? AND book_hash IN (sqlc.slice(hashes));

-- name: GetConfig :one
SELECT * FROM book_configs WHERE user_id = ? AND book_hash = ?;

-- name: UpsertConfig :exec
INSERT INTO book_configs (
  user_id, book_hash, meta_hash, location, xpointer, progress, rsvp_position, search_config, view_settings,
  tts_packs, created_at, updated_at, deleted_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (user_id, book_hash) DO UPDATE SET
  meta_hash = excluded.meta_hash, location = excluded.location, xpointer = excluded.xpointer,
  progress = excluded.progress, rsvp_position = excluded.rsvp_position, search_config = excluded.search_config,
  view_settings = excluded.view_settings, tts_packs = excluded.tts_packs, updated_at = excluded.updated_at,
  deleted_at = excluded.deleted_at;

-- name: PullConfigs :many
SELECT * FROM book_configs
WHERE user_id = sqlc.arg(user_id) AND (updated_at > sqlc.arg(since) OR deleted_at > sqlc.arg(since))
  AND ((sqlc.narg(book) IS NULL AND sqlc.narg(meta) IS NULL) OR book_hash = sqlc.narg(book) OR meta_hash = sqlc.narg(meta))
ORDER BY updated_at DESC;

-- name: GetNotesByIDs :many
SELECT * FROM book_notes WHERE user_id = ? AND id IN (sqlc.slice(ids));

-- name: UpsertNote :exec
INSERT INTO book_notes (
  user_id, book_hash, id, meta_hash, type, cfi, xpointer0, xpointer1, page, text, style, color, note, global,
  ink, crosspoint, created_at, updated_at, deleted_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (user_id, book_hash, id) DO UPDATE SET
  meta_hash = excluded.meta_hash, type = excluded.type, cfi = excluded.cfi, xpointer0 = excluded.xpointer0,
  xpointer1 = excluded.xpointer1, page = excluded.page, text = excluded.text, style = excluded.style,
  color = excluded.color, note = excluded.note, global = excluded.global, ink = excluded.ink,
  crosspoint = excluded.crosspoint, created_at = excluded.created_at, updated_at = excluded.updated_at,
  deleted_at = excluded.deleted_at;

-- name: PullNotes :many
SELECT * FROM book_notes
WHERE user_id = sqlc.arg(user_id) AND (updated_at > sqlc.arg(since) OR deleted_at > sqlc.arg(since))
  AND ((sqlc.narg(book) IS NULL AND sqlc.narg(meta) IS NULL) OR book_hash = sqlc.narg(book) OR meta_hash = sqlc.narg(meta))
ORDER BY updated_at DESC;
