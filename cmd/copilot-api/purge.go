package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
)

func acquireExistingStoreLock(store *sessionstore.Store) (*sessionstore.Lock, error) {
	if _, err := os.Stat(store.StateDir); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return sessionstore.AcquireLock(store.LockPath())
}

func purge(args []string) (returnErr error) {
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
	if err := cfg.ValidateDirs(); err != nil {
		return err
	}
	store := sessionstore.New(cfg.DataDir, cfg.StateDir, cfg.CacheDir)
	// Inventory is genuinely read-only: do not create the state directory or a
	// lock file for --dry-run.
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
	if err := cfg.EnsureConfigDir(); err != nil {
		return err
	}
	lifecycleLock, err := sessionstore.AcquireLock(sessionstore.LifecycleLockPath(cfg.ConfigDir))
	if err != nil {
		return fmt.Errorf("refusing to purge while server lifecycle lock is active: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, lifecycleLock.Release()) }()
	lock, err := acquireExistingStoreLock(store)
	if err != nil {
		return fmt.Errorf("refusing to purge while server lock is active: %w", err)
	}
	if lock != nil {
		defer func() {
			if lock != nil {
				returnErr = errors.Join(returnErr, lock.Release())
			}
		}()
	}
	if !*yes {
		fmt.Fprint(os.Stderr, "Type 'yes' to purge retained copilot-api data: ")
		var answer string
		_, _ = fmt.Fscan(os.Stdin, &answer)
		if answer != "yes" {
			return fmt.Errorf("purge cancelled")
		}
	}
	// The lifecycle lock still excludes server startup. Release the state lock
	// before deleting its containing directory; Windows cannot remove a file
	// while its lock handle remains open.
	if lock != nil {
		if err := lock.Release(); err != nil {
			return err
		}
		lock = nil
	}
	paths, err = store.Purge(false)
	if err != nil {
		return err
	}
	fmt.Printf("Purged %d directories.\n", len(paths))
	return nil
}
