package cache

import (
	"os"
	"path/filepath"
)

// CacheDir returns the root cache directory for a given remote.
func CacheDir(cacheBase, remoteID string) string {
	return filepath.Join(cacheBase, remoteID)
}

// FilePath returns the absolute path to a cached file on disk.
func FilePath(cacheBase, remoteID, relativePath string) string {
	return filepath.Join(cacheBase, remoteID, "files", relativePath)
}

// EnsureLayout creates the cache directory structure and returns the path to
// the SQLite metadata database.
func EnsureLayout(cacheBase, remoteID string) (dbPath string, err error) {
	filesDir := filepath.Join(cacheBase, remoteID, "files")
	if err := os.MkdirAll(filesDir, 0755); err != nil {
		return "", err
	}
	return filepath.Join(cacheBase, remoteID, "meta.db"), nil
}
