// Command bookroom runs the BookRoom server and its maintenance commands.
//
//	bookroom [serve]                       run the server
//	bookroom import [-shelf S] [-user U] PATH...   ingest files or directories
//	bookroom user add [-admin] [-email E] NAME     create an account (password from BOOKROOM_PASSWORD or stdin)
//	bookroom user passwd NAME                      set a password
//	bookroom user list                             list accounts
//	bookroom backup FILE                           copy the database while the server runs
//	bookroom version
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"bookroom/internal/auth"
	"bookroom/internal/config"
	"bookroom/internal/httpapi"
	"bookroom/internal/kosync"
	"bookroom/internal/library"
	"bookroom/internal/opds"
	"bookroom/internal/proxy"
	"bookroom/internal/readest"
	"bookroom/internal/scrape"
	"bookroom/internal/stats"
	"bookroom/internal/store"
)

// version is set at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "bookroom:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "version", "-v", "--version":
		fmt.Println(version)
		return nil
	case "help", "-h", "--help":
		fmt.Println(usage)
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	app, err := open(ctx)
	if err != nil {
		return err
	}
	defer app.store.Close()
	switch cmd {
	case "serve":
		return app.serve(ctx)
	case "import":
		return app.importCmd(ctx, args)
	case "user":
		return app.userCmd(ctx, args)
	case "backup":
		if len(args) != 1 {
			return errors.New("backup: give the target FILE (it must not exist)")
		}
		if err := app.store.Backup(ctx, args[0]); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
		fmt.Println("database copied to", args[0], "- also back up the library, covers and objects directories")
		return nil
	}
	return fmt.Errorf("unknown command %q\n%s", cmd, usage)
}

const usage = `usage:
  bookroom [serve]
  bookroom import [-shelf PATH] [-user NAME] FILE_OR_DIR...
  bookroom user add [-admin] [-email ADDR] NAME     (password: $BOOKROOM_PASSWORD or stdin)
  bookroom user passwd NAME                         (password: $BOOKROOM_PASSWORD or stdin)
  bookroom user list
  bookroom backup FILE
  bookroom version
Configuration comes from BOOKROOM_* environment variables (docs/operations.md).`

type app struct {
	cfg     *config.Config
	log     *slog.Logger
	store   *store.Store
	auth    *auth.Service
	lib     *library.Service
	scrape  *scrape.Service
	storage *readest.Storage
}

func newLogger(cfg *config.Config) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func open(ctx context.Context) (*app, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	log := newLogger(cfg)
	st, err := store.Open(ctx, cfg.DBPath())
	if err != nil {
		return nil, err
	}
	lib, err := library.New(st, cfg.DataDir, cfg.MaxUpload, cfg.ScrapeAuto)
	if err != nil {
		st.Close()
		return nil, err
	}
	storage, err := readest.NewStorage(st, lib, cfg.DataDir, cfg.JWTSecret)
	if err != nil {
		st.Close()
		return nil, err
	}
	a := &app{
		cfg: cfg, log: log, store: st, lib: lib, storage: storage,
		auth: auth.New(st, cfg.JWTSecret, cfg.AccessTTL, cfg.RefreshTTL, library.ProjectInto),
	}
	a.scrape = scrape.New(st, lib, cfg.ScrapeInterval, log,
		scrape.NewGoogle(cfg.GoogleBooksKey),
		scrape.NewOpenLibrary(cfg.OpenLibraryEnable, cfg.OpenLibraryUA),
		scrape.NewHardcover(cfg.HardcoverToken),
		scrape.NewDouban(cfg.DoubanEnable),
		scrape.NewXiaoshuohub(cfg.XSHEnable, cfg.XSHBaseURL, cfg.XSHAppKey, cfg.XSHAppSecret),
	)
	lib.OnScrapeQueued = a.scrape.Wake
	return a, nil
}

func (a *app) serve(ctx context.Context) error {
	if created, err := a.auth.EnsureAdmin(ctx, a.cfg.AdminUser, a.cfg.AdminPassword); err != nil {
		return fmt.Errorf("bootstrap admin: %w", err)
	} else if created {
		a.log.Info("created bootstrap admin", "user", a.cfg.AdminUser)
	}
	if n, _ := a.store.Q.CountUsers(ctx); n == 0 {
		a.log.Warn("no accounts yet: run `bookroom user add -admin NAME` or set BOOKROOM_ADMIN_USER/BOOKROOM_ADMIN_PASSWORD")
	}
	srv, err := httpapi.New(httpapi.Deps{
		Cfg: a.cfg, Store: a.store, Auth: a.auth, Lib: a.lib, Sync: readest.NewSync(a.store), Storage: a.storage,
		Replicas: readest.NewReplicas(a.store), Kosync: kosync.New(a.store), OPDS: &opds.Catalog{Lib: a.lib},
		Stats: stats.New(a.store), Scrape: a.scrape, Proxy: proxy.New(a.cfg.ProxyAllowPrivate, a.cfg.ProxyTrustedHosts),
		Log: a.log, Version: version,
	})
	if err != nil {
		return err
	}
	if a.cfg.ScrapeAuto {
		go a.scrape.Run(ctx)
	}
	go a.housekeeping(ctx)
	hs := &http.Server{Addr: a.cfg.Addr, Handler: srv.Handler(), ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 2 * time.Minute}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	a.log.Info("bookroom listening", "addr", a.cfg.Addr, "data", a.cfg.DataDir, "version", version)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	a.log.Info("shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return hs.Shutdown(shutdown)
}

// housekeeping removes expired refresh tokens and stale upload spools and refreshes planner statistics
// once an hour.
func (a *app) housekeeping(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		_ = a.store.WQ.DeleteExpiredRefreshTokens(ctx, time.Now().UnixMilli())
		if err := a.store.Optimize(ctx); err != nil && ctx.Err() == nil {
			a.log.Warn("optimize database", "err", err)
		}
		entries, _ := os.ReadDir(a.lib.TmpDir())
		for _, e := range entries {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
				os.Remove(filepath.Join(a.lib.TmpDir(), e.Name()))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (a *app) importCmd(ctx context.Context, args []string) error {
	fset := flag.NewFlagSet("import", flag.ContinueOnError)
	shelf := fset.String("shelf", "", "shelf for the imported books (for -user)")
	userName := fset.String("user", "", "account whose shelf receives the books")
	if err := fset.Parse(args); err != nil {
		return err
	}
	if fset.NArg() == 0 {
		return errors.New("import: give at least one file or directory")
	}
	opts := library.IngestOptions{}
	if *userName != "" {
		u, err := a.store.Q.GetUserByLogin(ctx, *userName)
		if err != nil {
			return fmt.Errorf("import: no user %q", *userName)
		}
		opts.AddedBy, opts.Shelf = u.ID, *shelf
	} else if *shelf != "" {
		return errors.New("import: -shelf needs -user")
	}
	var added, existing, failed int
	for _, root := range fset.Args() {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if d.IsDir() || strings.HasPrefix(d.Name(), ".") {
				return nil
			}
			if _, ok := library.FormatOf(path); !ok {
				return nil
			}
			b, created, err := a.lib.IngestFile(ctx, path, opts)
			switch {
			case err != nil:
				failed++
				fmt.Fprintf(os.Stderr, "FAIL  %s: %v\n", path, err)
			case created:
				added++
				fmt.Printf("ADD   %s  %s\n", b.Hash, path)
			default:
				existing++
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	fmt.Printf("imported %d, already present %d, failed %d\n", added, existing, failed)
	return a.store.Optimize(ctx)
}

func readPassword() (string, error) {
	if p := os.Getenv("BOOKROOM_PASSWORD"); p != "" {
		return p, nil
	}
	fmt.Fprint(os.Stderr, "password: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("no password given")
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (a *app) userCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "add":
		fset := flag.NewFlagSet("user add", flag.ContinueOnError)
		admin := fset.Bool("admin", false, "grant admin rights")
		email := fset.String("email", "", "e-mail address (also accepted as login)")
		if err := fset.Parse(args[1:]); err != nil {
			return err
		}
		if fset.NArg() != 1 {
			return errors.New("user add: give exactly one NAME")
		}
		pw, err := readPassword()
		if err != nil {
			return err
		}
		u, err := a.auth.CreateUser(ctx, fset.Arg(0), pw, *email, *admin)
		if err != nil {
			return err
		}
		fmt.Printf("created %s (%s) admin=%v\n", u.Username, u.ID, u.IsAdmin)
	case "passwd":
		if len(args) != 2 {
			return errors.New("user passwd: give exactly one NAME")
		}
		u, err := a.store.Q.GetUserByLogin(ctx, args[1])
		if err != nil {
			return fmt.Errorf("no user %q", args[1])
		}
		pw, err := readPassword()
		if err != nil {
			return err
		}
		if err := a.auth.SetPassword(ctx, u.ID, pw); err != nil {
			return err
		}
		fmt.Println("password updated")
	case "list":
		users, err := a.auth.ListUsers(ctx)
		if err != nil {
			return err
		}
		for _, u := range users {
			fmt.Printf("%s\t%s\tadmin=%v\t%s\n", u.ID, u.Username, u.IsAdmin, u.CreatedAt.Format(time.RFC3339))
		}
	default:
		return fmt.Errorf("unknown user command %q", args[0])
	}
	return nil
}
