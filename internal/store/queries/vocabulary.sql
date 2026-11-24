-- name: PullVocabulary :many
SELECT * FROM vocabulary
WHERE user_id = sqlc.arg(user_id)
  AND (updated_at > sqlc.arg(since_ts) OR (updated_at = sqlc.arg(since_ts) AND id > sqlc.arg(since_id)))
ORDER BY updated_at, id
LIMIT sqlc.arg(page_size);

-- name: GetVocabulary :many
SELECT * FROM vocabulary WHERE user_id = sqlc.arg(user_id) AND id IN (sqlc.slice(ids));

-- name: UpsertVocabulary :exec
INSERT INTO vocabulary (
  user_id, id, lang, word_key, word, state, hint, occurrences, created_at, modified_at, deleted_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (user_id, id) DO UPDATE SET
  lang = excluded.lang, word_key = excluded.word_key, word = excluded.word, state = excluded.state,
  hint = excluded.hint, occurrences = excluded.occurrences, created_at = excluded.created_at,
  modified_at = excluded.modified_at, deleted_at = excluded.deleted_at, updated_at = excluded.updated_at;
