-- name: UpsertStatBook :exec
INSERT INTO stat_books (user_id, book_hash, title, authors, updated_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (user_id, book_hash) DO UPDATE SET
  title = excluded.title, authors = excluded.authors, updated_at = excluded.updated_at;

-- name: DeleteStatBook :exec
INSERT INTO stat_books (user_id, book_hash, title, authors, updated_at, deleted_at)
VALUES (sqlc.arg(user_id), sqlc.arg(book_hash), sqlc.arg(title), sqlc.arg(authors), sqlc.arg(now), sqlc.arg(now))
ON CONFLICT (user_id, book_hash) DO UPDATE SET
  title = excluded.title, authors = excluded.authors, updated_at = excluded.updated_at, deleted_at = excluded.deleted_at;

-- name: UpsertStatPage :exec
INSERT INTO stat_pages (user_id, book_hash, page, start_time, duration, total_pages, ext, updated_at, deleted_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (user_id, book_hash, page, start_time) DO UPDATE SET
  duration = excluded.duration, total_pages = excluded.total_pages, ext = excluded.ext,
  updated_at = excluded.updated_at, deleted_at = excluded.deleted_at
WHERE excluded.duration > stat_pages.duration;

-- name: PullStatBooks :many
SELECT * FROM stat_books WHERE user_id = ? AND updated_at > ? ORDER BY updated_at;

-- name: DeletedStatBooks :many
SELECT * FROM stat_books WHERE user_id = ? AND deleted_at IS NOT NULL ORDER BY book_hash;

-- name: PullStatPages :many
SELECT * FROM stat_pages
WHERE user_id = sqlc.arg(user_id) AND updated_at > sqlc.arg(since)
  AND (sqlc.narg(book) IS NULL OR book_hash = sqlc.narg(book))
ORDER BY updated_at;

-- name: PullStatPagesPage :many
SELECT * FROM stat_pages
WHERE user_id = sqlc.arg(user_id) AND updated_at > sqlc.arg(since)
  AND (sqlc.narg(book) IS NULL OR book_hash = sqlc.narg(book))
ORDER BY updated_at
LIMIT sqlc.arg(lim);

-- name: StatPagesAt :many
SELECT * FROM stat_pages
WHERE user_id = sqlc.arg(user_id) AND updated_at = sqlc.arg(updated_at)
  AND (sqlc.narg(book) IS NULL OR book_hash = sqlc.narg(book));

-- name: StatEvents :many
-- Page events of live (not statistics-deleted) books between two epoch seconds.
SELECT p.book_hash, p.page, p.start_time, p.duration
FROM stat_pages p
LEFT JOIN stat_books b ON b.user_id = p.user_id AND b.book_hash = p.book_hash
WHERE p.user_id = sqlc.arg(user_id) AND p.start_time >= sqlc.arg(from_s) AND p.start_time < sqlc.arg(to_s)
  AND p.deleted_at IS NULL AND (b.deleted_at IS NULL OR p.start_time * 1000 >= b.deleted_at)
ORDER BY p.start_time;

-- name: StatBookTitles :many
SELECT book_hash, title, authors FROM stat_books WHERE user_id = ? AND book_hash IN (sqlc.slice(hashes));
