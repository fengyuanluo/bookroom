-- name: CreateUser :exec
INSERT INTO users (id, username, email, password_hash, is_admin, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetUser :one
SELECT * FROM users WHERE id = ?;

-- name: GetUserByLogin :one
SELECT * FROM users WHERE username = sqlc.arg(login) OR email = sqlc.arg(login) LIMIT 1;

-- name: ListUsers :many
SELECT * FROM users ORDER BY created_at;

-- name: ListUserIDs :many
SELECT id FROM users;

-- name: CountUsers :one
SELECT COUNT(*) FROM users;

-- name: DeleteUser :execrows
DELETE FROM users WHERE id = ?;

-- name: SetUserPassword :execrows
UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?;

-- name: InsertRefreshToken :exec
INSERT INTO refresh_tokens (token_hash, user_id, expires_at, created_at) VALUES (?, ?, ?, ?);

-- name: TakeRefreshToken :one
DELETE FROM refresh_tokens WHERE token_hash = ? RETURNING user_id, expires_at;

-- name: DeleteRefreshToken :exec
DELETE FROM refresh_tokens WHERE token_hash = ?;

-- name: DeleteUserRefreshTokens :exec
DELETE FROM refresh_tokens WHERE user_id = ?;

-- name: DeleteExpiredRefreshTokens :exec
DELETE FROM refresh_tokens WHERE expires_at < ?;

-- name: CreateDevice :exec
INSERT INTO devices (id, user_id, name, key_hash, created_at) VALUES (?, ?, ?, ?, ?);

-- name: ListDevices :many
SELECT * FROM devices WHERE user_id = ? ORDER BY created_at;

-- name: DeleteDevice :execrows
DELETE FROM devices WHERE id = ? AND user_id = ?;

-- name: GetDeviceUser :one
SELECT d.id AS device_id, u.*
FROM devices d JOIN users u ON u.id = d.user_id
WHERE d.key_hash = ?;

-- name: TouchDevice :exec
UPDATE devices SET last_used_at = ? WHERE id = ?;
