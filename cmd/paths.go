package cmd

import (
	"os"
	"path/filepath"
)

// AppRoot is the on-disk root for all odoo-cli state. Defaults to
// ~/.odoo; override with ODOO_CLI_ROOT for tests.
func AppRoot() string {
	if r := os.Getenv("ODOO_CLI_ROOT"); r != "" {
		return r
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// Last-resort fallback. Should not happen on any normal system.
		return ".odoo"
	}
	return filepath.Join(home, ".odoo")
}

// DatabasesDir holds one .env file per configured Odoo database.
func DatabasesDir() string { return filepath.Join(AppRoot(), "databases") }

// DatabaseEnvPath returns the .env path for a given database name.
func DatabaseEnvPath(name string) string {
	return filepath.Join(DatabasesDir(), name+".env")
}

// CacheDir is the per-database cache root.
func CacheDir(name string) string { return filepath.Join(AppRoot(), "cache", name) }

// JournalsCacheDir holds one JSON file per cached journal.
func JournalsCacheDir(name string) string {
	return filepath.Join(CacheDir(name), "journals")
}

// FavoritesPath is the per-database favorites file.
func FavoritesPath(name string) string {
	return filepath.Join(CacheDir(name), "favorites.json")
}

// PendingDir holds queued local changes waiting to be pushed.
func PendingDir(name string) string {
	return filepath.Join(CacheDir(name), "pending")
}

// SentDir archives successfully-pushed changes.
func SentDir(name string) string {
	return filepath.Join(CacheDir(name), "sent")
}

// LastSyncPath is the per-database sync cursor file.
func LastSyncPath(name string) string {
	return filepath.Join(CacheDir(name), "_last_sync.json")
}

// StateFilePath is the global state file (active DB + last-used).
func StateFilePath() string { return filepath.Join(AppRoot(), "state.json") }

// KeysDir is reserved for future signing material; mirror the
// SSH convention of a dedicated directory with restrictive perms.
// Currently unused but pre-created so the operator can drop keys
// there without worrying about cache rsyncs touching them.
func KeysDir() string { return filepath.Join(AppRoot(), "keys") }

// EnsureAppRoot creates the standard directory tree (chmod 0700 for
// privacy — these files contain credentials and operational state).
// Idempotent.
func EnsureAppRoot() error {
	for _, dir := range []string{
		AppRoot(),
		DatabasesDir(),
		KeysDir(),
	} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	return nil
}

// EnsureCacheDirs creates the per-database cache tree.
func EnsureCacheDirs(name string) error {
	for _, dir := range []string{
		CacheDir(name),
		JournalsCacheDir(name),
		PendingDir(name),
		SentDir(name),
	} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	return nil
}
