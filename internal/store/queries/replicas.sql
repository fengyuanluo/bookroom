-- name: GetReplica :one
SELECT * FROM replicas WHERE user_id = ? AND kind = ? AND replica_id = ?;

-- name: UpsertReplica :exec
INSERT INTO replicas (
  user_id, kind, replica_id, fields_jsonb, manifest_jsonb, deleted_at_ts, reincarnation, updated_at_ts,
  schema_version, created_at, modified_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (user_id, kind, replica_id) DO UPDATE SET
  fields_jsonb = excluded.fields_jsonb, manifest_jsonb = excluded.manifest_jsonb,
  deleted_at_ts = excluded.deleted_at_ts, reincarnation = excluded.reincarnation,
  updated_at_ts = excluded.updated_at_ts, schema_version = excluded.schema_version,
  modified_at = excluded.modified_at;

-- name: PullReplicas :many
SELECT * FROM replicas
WHERE user_id = sqlc.arg(user_id) AND kind = sqlc.arg(kind)
  AND (sqlc.narg(since) IS NULL OR updated_at_ts > sqlc.narg(since))
ORDER BY updated_at_ts
LIMIT 1000;

-- name: ListUserReplicas :many
SELECT * FROM replicas WHERE user_id = ?;

-- name: SetReplicaFields :exec
UPDATE replicas SET fields_jsonb = ?, modified_at = ? WHERE user_id = ? AND kind = ? AND replica_id = ?;

-- name: ListReplicaKeys :many
SELECT * FROM replica_keys WHERE user_id = ? ORDER BY created_at;

-- name: CreateReplicaKey :exec
INSERT INTO replica_keys (user_id, salt_id, alg, salt, created_at) VALUES (?, ?, ?, ?, ?);

-- name: DeleteReplicaKeys :exec
DELETE FROM replica_keys WHERE user_id = ?;
