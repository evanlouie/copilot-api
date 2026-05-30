package main

import (
	"flag"
	"fmt"
	"os"

	"copilot-api/internal/config"
	"copilot-api/internal/sessionstore"
)

func purge(args []string) error {
	fs := flag.NewFlagSet("purge", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "show what would be removed without deleting")
	yes := fs.Bool("yes", false, "confirm deletion without an interactive prompt")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store := sessionstore.New(cfg.DataDir, cfg.StateDir, cfg.CacheDir)
	lock, err := sessionstore.AcquireLock(store.LockPath())
	if err != nil {
		return fmt.Errorf("refusing to purge while server lock is active: %w", err)
	}
	defer lock.Release()
	paths, err := store.Purge(true)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		fmt.Println("Nothing to purge.")
		return nil
	}
	fmt.Println("The following directories will be removed:")
	for _, path := range paths {
		fmt.Println("  ", path)
	}
	if *dryRun {
		fmt.Println("Dry run only; no files removed.")
		return nil
	}
	if !*yes {
		fmt.Fprint(os.Stderr, "Type 'yes' to purge retained copilot-api data: ")
		var answer string
		_, _ = fmt.Fscan(os.Stdin, &answer)
		if answer != "yes" {
			return fmt.Errorf("purge cancelled")
		}
	}
	paths, err = store.Purge(false)
	if err != nil {
		return err
	}
	fmt.Printf("Purged %d directories.\n", len(paths))
	return nil
}
