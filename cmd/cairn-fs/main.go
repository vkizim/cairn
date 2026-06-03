// Command cairn-fs is a small demo CLI for Cairn's step-2 FS engine: it drives
// the repo package end-to-end against a Postgres database and a block store.
//
// Usage:
//
//	cairn-fs [flags] <subcommand> [args]
//
// Flags (must precede the subcommand):
//
//	-db      Postgres DSN (default: $CAIRN_TEST_DATABASE_URL, or from .env)
//	-store   block store directory (default ./cairn-store)
//	-backend block store backend: badger|fs (default badger)
//
// Subcommands:
//
//	create-library <name> <owner>   create a library, print its UUID
//	commit <library-id> <path>      ingest a file or directory into a new commit
//	ls <library-id> [path]          list a directory from path_index (default /)
//	log <library-id>                print commit history (newest first)
//	fsck <library-id>               verify the library and print a report
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/vkizim/cairn/blockstore"
	"github.com/vkizim/cairn/repo"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "cairn-fs:", err)
		os.Exit(1)
	}
}

func run() error {
	_ = repo.LoadDotEnv(".env")

	dbURL := flag.String("db", os.Getenv("CAIRN_TEST_DATABASE_URL"), "Postgres DSN")
	storePath := flag.String("store", "./cairn-store", "block store directory")
	backend := flag.String("backend", "badger", "block store backend: badger|fs")
	flag.Usage = usage
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		return fmt.Errorf("missing subcommand")
	}
	if *dbURL == "" {
		return fmt.Errorf("no database configured: pass -db or set CAIRN_TEST_DATABASE_URL (e.g. in .env)")
	}

	store, err := openStore(*backend, *storePath)
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	db, err := repo.Open(ctx, *dbURL, store)
	if err != nil {
		return err
	}
	defer db.Close()

	switch args[0] {
	case "create-library":
		return cmdCreateLibrary(ctx, db, args[1:])
	case "commit":
		return cmdCommit(ctx, db, args[1:])
	case "ls":
		return cmdLs(ctx, db, args[1:])
	case "log":
		return cmdLog(ctx, db, args[1:])
	case "fsck":
		return cmdFsck(ctx, db, args[1:])
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func usage() {
	fmt.Fprint(flag.CommandLine.Output(), `usage: cairn-fs [flags] <subcommand> [args]

flags (before the subcommand):
  -db       Postgres DSN (default $CAIRN_TEST_DATABASE_URL or .env)
  -store    block store directory (default ./cairn-store)
  -backend  badger|fs (default badger)

subcommands:
  create-library <name> <owner>
  commit <library-id> <path>
  ls <library-id> [path]
  log <library-id>
  fsck <library-id>
`)
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

func cmdCreateLibrary(ctx context.Context, db *repo.DB, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("create-library <name> <owner-username>")
	}
	// The owner must be an existing user (create one via `cairn-server create-user`).
	owner, err := db.GetUserByUsername(ctx, args[1])
	if err != nil {
		return fmt.Errorf("owner %q: %w (create the user first via cairn-server create-user)", args[1], err)
	}
	lib, err := db.CreateLibrary(ctx, args[0], owner.ID)
	if err != nil {
		return err
	}
	fmt.Println(lib.ID)
	return nil
}

func cmdCommit(ctx context.Context, db *repo.DB, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("commit <library-id> <path>")
	}
	libID, err := uuid.Parse(args[0])
	if err != nil {
		return fmt.Errorf("invalid library id: %w", err)
	}

	inputs, closeAll, err := collectInputs(args[1])
	if err != nil {
		return err
	}
	defer closeAll()
	if len(inputs) == 0 {
		return fmt.Errorf("no files found at %q", args[1])
	}

	start := time.Now()
	res, err := db.CommitFiles(ctx, libID, inputs, "commit "+filepath.Base(args[1]))
	if err != nil {
		return err
	}

	fmt.Printf("commit %s\n", res.Commit.Hash)
	fmt.Printf("  files:    %d\n", res.Stats.Files)
	fmt.Printf("  blocks:   %d total, %d unique\n", res.Stats.TotalBlocks, res.Stats.UniqueBlocks)
	fmt.Printf("  logical:  %s\n", humanBytes(res.Stats.LogicalBytes))
	fmt.Printf("  physical: %s (newly written)\n", humanBytes(res.Stats.PhysicalNewBytes))
	fmt.Printf("  dedup:    %s, saved %.1f%%\n", formatRatio(res.Stats.DedupRatio()), res.Stats.SavedFraction()*100)
	fmt.Printf("  elapsed:  %s\n", time.Since(start).Round(time.Millisecond))
	return nil
}

func cmdLs(ctx context.Context, db *repo.DB, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("ls <library-id> [path]")
	}
	libID, err := uuid.Parse(args[0])
	if err != nil {
		return fmt.Errorf("invalid library id: %w", err)
	}
	dir := "/"
	if len(args) >= 2 {
		dir = args[1]
	}
	entries, err := db.ListDir(ctx, libID, dir)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Printf("(empty: %s)\n", dir)
		return nil
	}
	for _, e := range entries {
		if e.IsDir {
			fmt.Printf("d         %s/\n", e.Name)
		} else {
			fmt.Printf("f %8s  %s\n", humanBytes(uint64(e.Size)), e.Name)
		}
	}
	return nil
}

func cmdLog(ctx context.Context, db *repo.DB, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("log <library-id>")
	}
	libID, err := uuid.Parse(args[0])
	if err != nil {
		return fmt.Errorf("invalid library id: %w", err)
	}
	history, err := db.History(ctx, libID)
	if err != nil {
		return err
	}
	if len(history) == 0 {
		fmt.Println("(no commits)")
		return nil
	}
	for _, c := range history {
		fmt.Printf("%s  %s  %s\n", c.Hash, c.Ctime.Format(time.RFC3339), c.Description)
	}
	return nil
}

func cmdFsck(ctx context.Context, db *repo.DB, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("fsck <library-id>")
	}
	libID, err := uuid.Parse(args[0])
	if err != nil {
		return fmt.Errorf("invalid library id: %w", err)
	}
	report, err := db.Fsck(ctx, libID)
	if err != nil {
		return err
	}
	head := "(none)"
	if report.HeadCommit != nil {
		head = report.HeadCommit.String()
	}
	fmt.Printf("library:        %s\n", report.LibraryID)
	fmt.Printf("head:           %s\n", head)
	fmt.Printf("head consistent: %t\n", report.HeadConsistent)
	if report.LastConsistentCommit != nil {
		fmt.Printf("last consistent: %s\n", report.LastConsistentCommit)
	}
	if len(report.Problems) > 0 {
		fmt.Printf("problems (%d):\n", len(report.Problems))
		for _, p := range report.Problems {
			fmt.Printf("  - %s\n", p)
		}
	} else {
		fmt.Println("problems:        none")
	}
	return nil
}

// collectInputs builds the commit's file set from a path (a single file or a
// directory walked recursively). Repository paths are relative to the given
// root, using forward slashes. The returned closer closes every opened file.
func collectInputs(root string) ([]repo.FileInput, func(), error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, func() {}, err
	}

	var (
		inputs  []repo.FileInput
		opened  []*os.File
		closeFn = func() {
			for _, f := range opened {
				_ = f.Close()
			}
		}
	)

	add := func(diskPath, repoPath string) error {
		f, err := os.Open(diskPath)
		if err != nil {
			return err
		}
		opened = append(opened, f)
		inputs = append(inputs, repo.FileInput{Path: repoPath, Reader: f})
		return nil
	}

	if !info.IsDir() {
		if err := add(root, filepath.Base(root)); err != nil {
			closeFn()
			return nil, func() {}, err
		}
		return inputs, closeFn, nil
	}

	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		return add(p, filepath.ToSlash(rel))
	})
	if err != nil {
		closeFn()
		return nil, func() {}, err
	}
	return inputs, closeFn, nil
}

func formatRatio(r float64) string {
	switch {
	case r == 0:
		return "n/a"
	case r > 1e9:
		return "∞ (all deduped)"
	default:
		return fmt.Sprintf("%.2fx", r)
	}
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
