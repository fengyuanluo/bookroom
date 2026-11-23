-- name: InsertLibraryBook :exec
INSERT INTO library_books (
  hash, format, size, file_name, original_filename, filename_md5, title, authors, series, series_index,
  description, publisher, published, language, isbn, tags, page_count, work_type, webnovel, cover_file,
  cover_hash, locked_fields, metadata_source, added_by, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetLibraryBook :one
SELECT * FROM library_books WHERE hash = ?;

-- name: ListLibraryBooks :many
SELECT * FROM library_books ORDER BY created_at;

-- name: GetLibraryBooksByHashes :many
SELECT * FROM library_books WHERE hash IN (sqlc.slice(hashes));

-- name: UpdateLibraryMetadata :exec
UPDATE library_books SET
  title = ?, authors = ?, series = ?, series_index = ?, description = ?, publisher = ?, published = ?,
  language = ?, isbn = ?, tags = ?, page_count = ?, work_type = ?, webnovel = ?, cover_file = ?, cover_hash = ?,
  locked_fields = ?, metadata_source = ?, updated_at = ?
WHERE hash = ?;

-- name: DeleteLibraryBook :execrows
DELETE FROM library_books WHERE hash = ?;

-- name: LibraryHashesByFilenameMD5 :many
SELECT hash FROM library_books WHERE filename_md5 = ?;

-- name: EnqueueScrapeJob :exec
INSERT INTO scrape_jobs (hash, status, attempts, next_run_at, last_error, updated_at)
VALUES (?, 'pending', 0, ?, '', ?)
ON CONFLICT (hash) DO UPDATE SET status = 'pending', attempts = 0, next_run_at = excluded.next_run_at,
  last_error = '', updated_at = excluded.updated_at;

-- name: GetScrapeJob :one
SELECT * FROM scrape_jobs WHERE hash = ?;

-- name: NextDueScrapeJob :one
SELECT * FROM scrape_jobs WHERE status = 'pending' AND next_run_at <= ? ORDER BY next_run_at LIMIT 1;

-- name: UpdateScrapeJob :exec
UPDATE scrape_jobs SET status = ?, attempts = ?, next_run_at = ?, last_error = ?, updated_at = ? WHERE hash = ?;

-- name: GetProviderState :one
SELECT * FROM provider_state WHERE provider = ?;

-- name: SetProviderPausedUntil :exec
INSERT INTO provider_state (provider, paused_until, updated_at) VALUES (?, ?, ?)
ON CONFLICT (provider) DO UPDATE SET paused_until = excluded.paused_until, updated_at = excluded.updated_at;
