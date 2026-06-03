// Command cairn-server runs the Cairn HTTP API and provides out-of-band admin
// subcommands.
//
// Usage:
//
//	cairn-server [flags] [serve|create-user <username>|sweep-uploads]
//
// Flags / env:
//
//	-addr     / CAIRN_LISTEN_ADDR     listen address (default :8080)
//	-db       / CAIRN_DATABASE_URL    Postgres DSN (falls back to CAIRN_TEST_DATABASE_URL)
//	-store    / CAIRN_STORE           block store directory (default ./cairn-store)
//	-backend  / CAIRN_STORE_BACKEND   badger|fs (default badger)
//	            CAIRN_DEV             if set, relaxes the Secure cookie flag for http://localhost
//
// create-user reads the password from CAIRN_NEW_USER_PASSWORD or, if absent,
// interactively from the terminal. The password is never logged.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/vkizim/cairn/api"
	"github.com/vkizim/cairn/blockstore"
	"github.com/vkizim/cairn/repo"
	"github.com/vkizim/cairn/web"
)

// newFlagSet binds CLI flags onto cfg, using the env-derived values as defaults
// so an explicit flag overrides the environment.
func newFlagSet(cfg *config) *flag.FlagSet {
	fs := flag.NewFlagSet("cairn-server", flag.ContinueOnError)
	fs.StringVar(&cfg.addr, "addr", cfg.addr, "listen address")
	fs.StringVar(&cfg.dbURL, "db", cfg.dbURL, "Postgres DSN")
	fs.StringVar(&cfg.store, "store", cfg.store, "block store directory")
	fs.StringVar(&cfg.backend, "backend", cfg.backend, "block store backend: badger|fs")
	return fs
}

type config struct {
	addr    string
	dbURL   string
	store   string
	backend string
	dev     bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "cairn-server:", err)
		os.Exit(1)
	}
}

func run() error {
	_ = repo.LoadDotEnv(".env")

	cfg := config{
		addr:    envOr("CAIRN_LISTEN_ADDR", ":8080"),
		dbURL:   envOr("CAIRN_DATABASE_URL", os.Getenv("CAIRN_TEST_DATABASE_URL")),
		store:   envOr("CAIRN_STORE", "./cairn-store"),
		backend: envOr("CAIRN_STORE_BACKEND", "badger"),
		dev:     os.Getenv("CAIRN_DEV") != "",
	}

	fs := newFlagSet(&cfg)
	_ = fs.Parse(os.Args[1:])
	args := fs.Args()

	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
		args = args[1:]
	}
	if cfg.dbURL == "" {
		return errors.New("no database configured: set CAIRN_DATABASE_URL / CAIRN_TEST_DATABASE_URL or -db")
	}

	switch cmd {
	case "serve":
		return cmdServe(cfg)
	case "create-user":
		return cmdCreateUser(cfg, args)
	case "sweep-uploads":
		return cmdSweep(cfg)
	default:
		return fmt.Errorf("unknown subcommand %q (want serve|create-user|sweep-uploads)", cmd)
	}
}

func cmdServe(cfg config) error {
	ctx := context.Background()
	store, err := openStore(cfg.backend, cfg.store)
	if err != nil {
		return err
	}
	defer store.Close()

	db, err := repo.Open(ctx, cfg.dbURL, store)
	if err != nil {
		return err
	}
	defer db.Close()

	apiCfg := api.Config{Dev: cfg.dev}
	if fsys, ok := web.FS(); ok {
		apiCfg.Static = api.SPAFileServer(fsys)
		fmt.Println("serving embedded SPA")
	}
	srv, err := api.NewServer(db, store, apiCfg)
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:              cfg.addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	idle := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		close(idle)
	}()

	if cfg.dev {
		fmt.Printf("cairn-server (dev) listening on %s\n", cfg.addr)
	} else {
		fmt.Printf("cairn-server listening on %s\n", cfg.addr)
	}
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-idle
	return nil
}

func cmdCreateUser(cfg config, args []string) error {
	if len(args) != 1 {
		return errors.New("create-user <username>")
	}
	username := strings.TrimSpace(args[0])
	if username == "" {
		return errors.New("username must not be empty")
	}

	password, err := readPassword()
	if err != nil {
		return err
	}
	if password == "" {
		return errors.New("password must not be empty")
	}
	hash, err := api.HashPassword(password)
	if err != nil {
		return err
	}

	ctx := context.Background()
	db, err := repo.Open(ctx, cfg.dbURL, nil) // block store not needed for user admin
	if err != nil {
		return err
	}
	defer db.Close()

	user, err := db.CreateUser(ctx, username, hash)
	if errors.Is(err, repo.ErrUsernameTaken) {
		return fmt.Errorf("username %q already exists", username)
	}
	if err != nil {
		return err
	}
	fmt.Printf("created user %s (%s)\n", user.Username, user.ID)
	return nil
}

func cmdSweep(cfg config) error {
	ctx := context.Background()
	db, err := repo.Open(ctx, cfg.dbURL, nil)
	if err != nil {
		return err
	}
	defer db.Close()

	uploads, err := db.SweepExpiredUploads(ctx)
	if err != nil {
		return err
	}
	sessions, err := db.SweepExpiredSessions(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("swept %d expired upload(s), %d expired session(s)\n", uploads, sessions)
	return nil
}

// readPassword reads the new user's password from CAIRN_NEW_USER_PASSWORD or,
// if unset, prompts on the terminal without echo. The value is never logged.
func readPassword() (string, error) {
	if p := os.Getenv("CAIRN_NEW_USER_PASSWORD"); p != "" {
		return p, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("no terminal: set CAIRN_NEW_USER_PASSWORD")
	}
	fmt.Print("New password: ")
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func openStore(backend, path string) (blockstore.Store, error) {
	switch backend {
	case "badger":
		return blockstore.NewBadgerStore(path)
	case "fs":
		return blockstore.NewFSStore(path)
	default:
		return nil, fmt.Errorf("unknown backend %q (want badger|fs)", backend)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
