-- name: GetFile :one
SELECT * FROM files WHERE user_id = ? AND file_key = ?;

-- name: ReserveFile :exec
INSERT INTO files (user_id, file_key, book_hash, replica_kind, replica_id, file_size, stored, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?)
ON CONFLICT (user_id, file_key) DO NOTHING;

-- name: MarkFileStored :exec
UPDATE files SET stored = 1, file_size = ?, updated_at = ? WHERE user_id = ? AND file_key = ?;

-- name: DeleteFile :exec
DELETE FROM files WHERE user_id = ? AND file_key = ?;

-- name: StoredFilesByKeys :many
SELECT * FROM files WHERE user_id = ? AND stored = 1 AND file_key IN (sqlc.slice(keys));

-- name: StoredFilesByBookHashes :many
SELECT * FROM files WHERE user_id = ? AND stored = 1 AND book_hash IN (sqlc.slice(hashes));

-- name: StoredFilesByReplicaIDs :many
SELECT * FROM files WHERE user_id = ? AND stored = 1 AND replica_id IN (sqlc.slice(ids));

-- name: StorageUsage :one
SELECT CAST(COALESCE(SUM(file_size), 0) AS INTEGER) FROM files WHERE user_id = ? AND stored = 1;
