-- Readest's vocabulary builder sync (v0.12.20 `/api/sync/vocabulary`, Supabase migration 032).
-- One row per looked-up word. `updated_at` (epoch ms) is stamped by the server on every write and is the pull
-- cursor, paged by the (updated_at, id) keyset; created_at / modified_at / deleted_at are the clients' clocks.

-- +goose Up
CREATE TABLE vocabulary (
  user_id     TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  id          TEXT NOT NULL,
  lang        TEXT NOT NULL,
  word_key    TEXT NOT NULL,
  word        TEXT NOT NULL,
  state       TEXT NOT NULL DEFAULT 'none' CHECK (state IN ('none', 'learning', 'mastered')),
  hint        TEXT,
  occurrences TEXT NOT NULL DEFAULT '[]',
  created_at  INTEGER NOT NULL,
  modified_at INTEGER NOT NULL,
  deleted_at  INTEGER,
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY (user_id, id)
);
CREATE INDEX vocabulary_cursor ON vocabulary (user_id, updated_at, id);

-- +goose Down
DROP TABLE vocabulary;
