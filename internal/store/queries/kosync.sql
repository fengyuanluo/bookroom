-- name: GetKosyncProgress :one
SELECT * FROM kosync_progress WHERE user_id = ? AND document = ?;

-- name: UpsertKosyncProgress :exec
INSERT INTO kosync_progress (user_id, document, progress, percentage, device, device_id, timestamp)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (user_id, document) DO UPDATE SET
  progress = excluded.progress, percentage = excluded.percentage, device = excluded.device,
  device_id = excluded.device_id, timestamp = excluded.timestamp;

-- name: GetKosyncLink :one
SELECT book_hash FROM kosync_links WHERE user_id = ? AND document = ?;

-- name: SaveKosyncLink :exec
INSERT INTO kosync_links (user_id, document, book_hash) VALUES (?, ?, ?)
ON CONFLICT (user_id, document) DO NOTHING;

-- name: UserBookHashesByFilenameMD5 :many
SELECT b.book_hash FROM books b JOIN library_books l ON l.hash = b.book_hash
WHERE b.user_id = ? AND b.deleted_at IS NULL AND l.filename_md5 = ?
ORDER BY b.updated_at DESC;

-- name: UserBooksByTitle :many
SELECT book_hash, title, source_title, author FROM books
WHERE user_id = sqlc.arg(user_id) AND deleted_at IS NULL AND format = 'EPUB'
  AND (lower(title) = lower(sqlc.arg(title)) OR lower(source_title) = lower(sqlc.arg(title)))
ORDER BY updated_at DESC;
