-- BookRoom schema. Times are UTC epoch milliseconds (INTEGER); see docs/adr/0003-sqlite-only.md.
-- Readest-compatible tables (books, book_configs, book_notes, stat_*, replicas, replica_keys, files) keep
-- Readest's column names so the wire format maps 1:1.

-- +goose Up

CREATE TABLE users (
  id            TEXT PRIMARY KEY,
  username      TEXT NOT NULL UNIQUE COLLATE NOCASE,
  email         TEXT UNIQUE COLLATE NOCASE,
  password_hash TEXT NOT NULL,
  is_admin      INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);

CREATE TABLE refresh_tokens (
  token_hash TEXT PRIMARY KEY,
  user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  expires_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX refresh_tokens_user ON refresh_tokens (user_id);

-- KOSync / OPDS device keys. key_hash = sha256(hex(md5(key))): KOSync clients send md5(key) as x-auth-key.
CREATE TABLE devices (
  id           TEXT PRIMARY KEY,
  user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  name         TEXT NOT NULL,
  key_hash     TEXT NOT NULL UNIQUE,
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER
);
CREATE INDEX devices_user ON devices (user_id);

-- The shared library: one row per file, keyed by partial MD5 (docs/adr/0002-content-addressed-library.md).
CREATE TABLE library_books (
  hash              TEXT PRIMARY KEY,
  format            TEXT NOT NULL,
  size              INTEGER NOT NULL,
  file_name         TEXT NOT NULL,
  original_filename TEXT NOT NULL,
  filename_md5      TEXT NOT NULL,
  title             TEXT NOT NULL,
  authors           TEXT NOT NULL DEFAULT '[]',
  series            TEXT NOT NULL DEFAULT '',
  series_index      REAL,
  description       TEXT NOT NULL DEFAULT '',
  publisher         TEXT NOT NULL DEFAULT '',
  published         TEXT NOT NULL DEFAULT '',
  language          TEXT NOT NULL DEFAULT '',
  isbn              TEXT NOT NULL DEFAULT '',
  tags              TEXT NOT NULL DEFAULT '[]',
  page_count        INTEGER,
  work_type         TEXT NOT NULL DEFAULT 'unknown',
  webnovel          TEXT,
  cover_file        TEXT NOT NULL DEFAULT '',
  cover_hash        TEXT NOT NULL DEFAULT '',
  locked_fields     TEXT NOT NULL DEFAULT '[]',
  metadata_source   TEXT NOT NULL DEFAULT '',
  added_by          TEXT,
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL
);
CREATE INDEX library_books_created ON library_books (created_at);
CREATE INDEX library_books_filename_md5 ON library_books (filename_md5);

-- Readest `books`: one row per user and book hash. Library books are projected into every user's rows.
CREATE TABLE books (
  user_id                   TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  book_hash                 TEXT NOT NULL,
  meta_hash                 TEXT,
  format                    TEXT NOT NULL DEFAULT '',
  title                     TEXT NOT NULL DEFAULT '',
  source_title              TEXT,
  original_filename         TEXT,
  author                    TEXT NOT NULL DEFAULT '',
  group_id                  TEXT,
  group_name                TEXT,
  group_updated_at          INTEGER,
  tags                      TEXT,
  progress                  TEXT,
  reading_status            TEXT,
  reading_status_updated_at INTEGER,
  cover_hash                TEXT,
  cover_updated_at          INTEGER,
  page_count                INTEGER,
  metadata                  TEXT,
  metadata_updated_at       INTEGER,
  created_at                INTEGER,
  updated_at                INTEGER,
  deleted_at                INTEGER,
  uploaded_at               INTEGER,
  synced_at                 INTEGER NOT NULL,
  PRIMARY KEY (user_id, book_hash)
);
CREATE INDEX books_synced ON books (user_id, synced_at);
CREATE INDEX books_meta ON books (user_id, meta_hash);

CREATE TABLE book_configs (
  user_id       TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  book_hash     TEXT NOT NULL,
  meta_hash     TEXT,
  location      TEXT,
  xpointer      TEXT,
  progress      TEXT,
  rsvp_position TEXT,
  search_config TEXT,
  view_settings TEXT,
  tts_packs     TEXT,
  created_at    INTEGER,
  updated_at    INTEGER,
  deleted_at    INTEGER,
  PRIMARY KEY (user_id, book_hash)
);
CREATE INDEX book_configs_updated ON book_configs (user_id, updated_at);

CREATE TABLE book_notes (
  user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  book_hash  TEXT NOT NULL,
  id         TEXT NOT NULL,
  meta_hash  TEXT,
  type       TEXT,
  cfi        TEXT,
  xpointer0  TEXT,
  xpointer1  TEXT,
  page       REAL,
  text       TEXT,
  style      TEXT,
  color      TEXT,
  note       TEXT,
  global     INTEGER,
  ink        TEXT,
  crosspoint TEXT,
  created_at INTEGER,
  updated_at INTEGER,
  deleted_at INTEGER,
  PRIMARY KEY (user_id, book_hash, id)
);
CREATE INDEX book_notes_updated ON book_notes (user_id, updated_at);
CREATE INDEX book_notes_id ON book_notes (user_id, id);

CREATE TABLE stat_books (
  user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  book_hash  TEXT NOT NULL,
  title      TEXT NOT NULL DEFAULT '',
  authors    TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL,
  deleted_at INTEGER,
  PRIMARY KEY (user_id, book_hash)
);
CREATE INDEX stat_books_updated ON stat_books (user_id, updated_at);

-- KOReader page events (docs/adr/0008-reading-stats-raw-events.md). start_time and duration are seconds.
CREATE TABLE stat_pages (
  user_id     TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  book_hash   TEXT NOT NULL,
  page        INTEGER NOT NULL,
  start_time  INTEGER NOT NULL,
  duration    INTEGER NOT NULL,
  total_pages INTEGER NOT NULL DEFAULT 0,
  ext         TEXT,
  updated_at  INTEGER NOT NULL,
  deleted_at  INTEGER,
  PRIMARY KEY (user_id, book_hash, page, start_time)
);
CREATE INDEX stat_pages_updated ON stat_pages (user_id, updated_at);
CREATE INDEX stat_pages_time ON stat_pages (user_id, start_time);

CREATE TABLE replicas (
  user_id        TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  kind           TEXT NOT NULL,
  replica_id     TEXT NOT NULL,
  fields_jsonb   TEXT NOT NULL DEFAULT '{}',
  manifest_jsonb TEXT,
  deleted_at_ts  TEXT,
  reincarnation  TEXT,
  updated_at_ts  TEXT NOT NULL,
  schema_version INTEGER NOT NULL DEFAULT 1,
  created_at     INTEGER NOT NULL,
  modified_at    INTEGER NOT NULL,
  PRIMARY KEY (user_id, kind, replica_id)
);
CREATE INDEX replicas_cursor ON replicas (user_id, kind, updated_at_ts);

CREATE TABLE replica_keys (
  user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  salt_id    TEXT NOT NULL,
  alg        TEXT NOT NULL,
  salt       BLOB NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (user_id, salt_id)
);

-- Per-user objects behind /api/storage (fonts, textures, replica files, user covers).
-- Library book files are not listed here; storage resolves them through library_books.
CREATE TABLE files (
  user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  file_key     TEXT NOT NULL,
  book_hash    TEXT,
  replica_kind TEXT,
  replica_id   TEXT,
  file_size    INTEGER NOT NULL,
  stored       INTEGER NOT NULL DEFAULT 0,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL,
  PRIMARY KEY (user_id, file_key)
);
CREATE INDEX files_book ON files (user_id, book_hash);
CREATE INDEX files_replica ON files (user_id, replica_id);

-- Native KOSync positions; EPUB XPointers are mirrored into book_configs.
CREATE TABLE kosync_progress (
  user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  document   TEXT NOT NULL,
  progress   TEXT NOT NULL,
  percentage REAL NOT NULL,
  device     TEXT NOT NULL DEFAULT '',
  device_id  TEXT NOT NULL DEFAULT '',
  timestamp  INTEGER NOT NULL,
  PRIMARY KEY (user_id, document)
);

-- A KOSync document id (e.g. a filename MD5) linked to the library book it stands for.
CREATE TABLE kosync_links (
  user_id   TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  document  TEXT NOT NULL,
  book_hash TEXT NOT NULL,
  PRIMARY KEY (user_id, document)
);

-- Background gap filling (docs/adr/0006-embedded-metadata-first.md).
CREATE TABLE scrape_jobs (
  hash        TEXT PRIMARY KEY REFERENCES library_books (hash) ON DELETE CASCADE,
  status      TEXT NOT NULL,
  attempts    INTEGER NOT NULL DEFAULT 0,
  next_run_at INTEGER NOT NULL,
  last_error  TEXT NOT NULL DEFAULT '',
  updated_at  INTEGER NOT NULL
);
CREATE INDEX scrape_jobs_due ON scrape_jobs (status, next_run_at);

-- A source paused until its quota window rolls over.
CREATE TABLE provider_state (
  provider     TEXT PRIMARY KEY,
  paused_until INTEGER NOT NULL DEFAULT 0,
  updated_at   INTEGER NOT NULL
);

-- +goose Down
DROP TABLE provider_state;
DROP TABLE scrape_jobs;
DROP TABLE kosync_links;
DROP TABLE kosync_progress;
DROP TABLE files;
DROP TABLE replica_keys;
DROP TABLE replicas;
DROP TABLE stat_pages;
DROP TABLE stat_books;
DROP TABLE book_notes;
DROP TABLE book_configs;
DROP TABLE books;
DROP TABLE library_books;
DROP TABLE devices;
DROP TABLE refresh_tokens;
DROP TABLE users;
