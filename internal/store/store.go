// Package store opens the SQLite database, applies migrations and hands out sqlc queries.
//
// SQLite allows one writer at a time, so writes go through a dedicated single-connection pool whose
// transactions start with BEGIN IMMEDIATE (no lock upgrades, no SQLITE_BUSY mid-transaction), while reads use
// a separate pool that WAL lets run concurrently with the writer.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"runtime"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"bookroom/internal/store/db"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store is the database handle shared by every service.
type Store struct {
	R  *sql.DB     // read pool
	W  *sql.DB     // single-connection write pool
	Q  *db.Queries // queries on the read pool
	WQ *db.Queries // queries on the write pool, outside a transaction
}

// Open opens (creating if needed) the database at path and migrates it to the latest schema.
func Open(ctx context.Context, path string) (*Store, error) {
	pragmas := url.Values{}
	for _, p := range []string{"busy_timeout(10000)", "journal_mode(WAL)", "synchronous(NORMAL)", "foreign_keys(1)", "cache_size(-32000)"} {
		pragmas.Add("_pragma", p)
	}
	w, err := sql.Open("sqlite", "file:"+path+"?"+pragmas.Encode()+"&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	r, err := sql.Open("sqlite", "file:"+path+"?"+pragmas.Encode())
	if err != nil {
		_ = w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(max(4, runtime.NumCPU()))
	s := &Store{R: r, W: w, Q: db.New(r), WQ: db.New(w)}
	if err := s.migrate(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate(ctx context.Context) error {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, s.W, sub)
	if err != nil {
		return fmt.Errorf("store: migrations: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

// Close closes both pools.
func (s *Store) Close() error {
	return errors.Join(s.R.Close(), s.W.Close())
}

// Tx runs fn in one write transaction, committing when fn returns nil.
func (s *Store) Tx(ctx context.Context, fn func(q *db.Queries) error) error {
	tx, err := s.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(db.New(tx)); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// IsNotFound reports whether err is a no-rows result.
func IsNotFound(err error) bool { return errors.Is(err, sql.ErrNoRows) }
