package main

import (
	"errors"
	"flag"
	"fmt"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
)

func configuredRetentionPolicy(cfg config.Config) sessionstore.RetentionPolicy {
	return sessionstore.RetentionPolicy{MaxAge: cfg.RetentionMaxAge, MaxResponses: cfg.RetentionMaxResponses, MaxBytes: cfg.RetentionMaxBytes}
}

func prune(args []string) (returnErr error) {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "show retained entries that would be removed")
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
	store.SetRetentionPolicy(configuredRetentionPolicy(cfg))
	if *dryRun {
		report, err := store.Prune(true)
		if err != nil {
			return err
		}
		for _, path := range report.Paths {
			fmt.Println(path)
		}
		fmt.Printf("Would prune %d entries (%d bytes).\n", len(report.Paths), report.Bytes)
		return nil
	}
	present, err := store.ValidatePruneRoots()
	if err != nil {
		return err
	}
	if !present {
		fmt.Println("Pruned 0 entries (0 bytes).")
		return nil
	}
	if err := cfg.EnsureConfigDir(); err != nil {
		return err
	}
	lifecycleLock, err := sessionstore.AcquireLock(sessionstore.LifecycleLockPath(cfg.ConfigDir))
	if err != nil {
		return fmt.Errorf("refusing to prune while server lifecycle lock is active: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, lifecycleLock.Release()) }()
	lock, err := acquireExistingStoreLock(store)
	if err != nil {
		return fmt.Errorf("refusing to prune while server lock is active: %w", err)
	}
	if lock != nil {
		defer func() { returnErr = errors.Join(returnErr, lock.Release()) }()
	}
	report, err := store.Prune(false)
	if err != nil {
		return err
	}
	for _, path := range report.Paths {
		fmt.Println(path)
	}
	fmt.Printf("Pruned %d entries (%d bytes).\n", len(report.Paths), report.Bytes)
	return nil
}
