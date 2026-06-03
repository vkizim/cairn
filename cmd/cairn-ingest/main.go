// Command cairn-ingest runs Cairn's chunker + block store over a file or
// directory and reports dedup statistics.
//
// Usage:
//
//	cairn-ingest [-backend badger|fs] [-store DIR] PATH
//
// PATH may be a single file or a directory (walked recursively). It prints,
// per file and in total: unique/total blocks, logical and physical-new bytes,
// dedup ratio, and elapsed time.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/vkizim/cairn/blockstore"
	"github.com/vkizim/cairn/ingest"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "cairn-ingest:", err)
		os.Exit(1)
	}
}

func run() error {
	backend := flag.String("backend", "badger", "block store backend: badger|fs")
	storePath := flag.String("store", "./cairn-store", "directory for the block store")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: %s [-backend badger|fs] [-store DIR] PATH\n", filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() != 1 {
		flag.Usage()
		return fmt.Errorf("expected exactly one PATH argument, got %d", flag.NArg())
	}
	target := flag.Arg(0)

	store, err := openStore(*backend, *storePath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			fmt.Fprintln(os.Stderr, "cairn-ingest: close store:", cerr)
		}
	}()

	files, err := collectFiles(target)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no files found at %q", target)
	}

	const ns = blockstore.DefaultNamespace
	start := time.Now()

	var (
		totalLogical, totalPhysical uint64
		totalBlocks                 int
		globalUnique                = make(map[blockstore.Hash]struct{})
	)

	fmt.Printf("Ingesting %d file(s) with backend=%s store=%s\n\n", len(files), *backend, *storePath)

	for _, path := range files {
		fileStart := time.Now()
		m, stats, ierr := ingestFile(store, ns, path)
		if ierr != nil {
			return fmt.Errorf("ingest %q: %w", path, ierr)
		}
		for _, b := range m.Blocks {
			globalUnique[b.Hash] = struct{}{}
		}
		totalLogical += stats.LogicalBytes
		totalPhysical += stats.PhysicalNewBytes
		totalBlocks += stats.TotalBlocks

		fmt.Printf("%s\n", path)
		printStats("  ", stats, time.Since(fileStart))
	}

	// Aggregate stats across all files. UniqueBlocks is the cross-file distinct
	// set, so identical files collapse to the same blocks here.
	total := ingest.Stats{
		LogicalBytes:     totalLogical,
		PhysicalNewBytes: totalPhysical,
		UniqueBlocks:     len(globalUnique),
		TotalBlocks:      totalBlocks,
	}
	fmt.Printf("\nTOTAL (%d files)\n", len(files))
	printStats("  ", total, time.Since(start))
	return nil
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

// collectFiles returns the regular files under target (a file or directory), in
// deterministic (sorted) order so runs are reproducible.
func collectFiles(target string) ([]string, error) {
	info, err := os.Stat(target)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{target}, nil
	}

	var files []string
	err = filepath.WalkDir(target, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

func ingestFile(store blockstore.Store, ns, path string) (ingest.Manifest, ingest.Stats, error) {
	f, err := os.Open(path)
	if err != nil {
		return ingest.Manifest{}, ingest.Stats{}, err
	}
	defer f.Close()

	// Buffer reads so the chunker isn't starved by tiny syscalls; it still
	// streams (we never hold the whole file).
	return ingest.Ingest(store, ns, io.Reader(f), path)
}

func printStats(indent string, s ingest.Stats, elapsed time.Duration) {
	fmt.Printf("%sblocks:   %d total, %d unique\n", indent, s.TotalBlocks, s.UniqueBlocks)
	fmt.Printf("%slogical:  %s\n", indent, humanBytes(s.LogicalBytes))
	fmt.Printf("%sphysical: %s (newly written)\n", indent, humanBytes(s.PhysicalNewBytes))
	fmt.Printf("%sdedup:    %s, saved %.1f%%\n", indent, formatRatio(s.DedupRatio()), s.SavedFraction()*100)
	fmt.Printf("%selapsed:  %s\n", indent, elapsed.Round(time.Millisecond))
}

func formatRatio(r float64) string {
	switch {
	case r == 0:
		return "n/a"
	case r > 1e9: // +Inf: everything deduped
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
	return fmt.Sprintf("%.2f %ciB (%d B)", float64(n)/float64(div), "KMGTPE"[exp], n)
}
