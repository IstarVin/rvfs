package cache

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// CacheLayer combines local-file I/O with SQLite metadata tracking.
// All state-modifying operations are wrapped in a transaction so the DB
// stays consistent with the on-disk cache.
type CacheLayer struct {
	db       *MetadataDB
	filesDir string // absolute path to <cacheBase>/<remoteID>/files/
}

// NewCacheLayer creates the cache directory structure, opens the metadata
// database, and returns a ready-to-use CacheLayer.
func NewCacheLayer(cacheBase, remoteID string) (*CacheLayer, error) {
	dbPath, err := EnsureLayout(cacheBase, remoteID)
	if err != nil {
		return nil, fmt.Errorf("ensure layout: %w", err)
	}

	db, err := OpenDB(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	return &CacheLayer{
		db:       db,
		filesDir: filepath.Join(cacheBase, remoteID, "files"),
	}, nil
}

// Close closes the underlying database.
func (c *CacheLayer) Close() error {
	return c.db.Close()
}

// DB returns the underlying MetadataDB for direct queries (e.g. in tests).
func (c *CacheLayer) DB() *MetadataDB {
	return c.db
}

// diskPath returns the absolute path on disk for a relative cache path.
func (c *CacheLayer) diskPath(rel string) string {
	if rel == "" {
		return c.filesDir
	}
	return filepath.Join(c.filesDir, rel)
}

// ---------- Read operations (from DB) ----------

// Stat returns the FileEntry for the given relative path, or nil if not found.
func (c *CacheLayer) Stat(path string) (*FileEntry, error) {
	return c.db.GetFile(path)
}

// ReadDir returns the immediate children of dirPath from the database.
func (c *CacheLayer) ReadDir(dirPath string) ([]*FileEntry, error) {
	return c.db.ListDir(dirPath)
}

// ---------- File I/O operations ----------

// Open opens the cache file with the given flags.
func (c *CacheLayer) Open(rel string, flags int) (*os.File, error) {
	return os.OpenFile(c.diskPath(rel), flags, 0)
}

// Read reads from the cache file at the given offset.
func (c *CacheLayer) Read(rel string, dest []byte, off int64) (int, error) {
	f, err := os.Open(c.diskPath(rel))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n, err := f.ReadAt(dest, off)
	if err == io.EOF {
		err = nil
	}
	// Best-effort last-access tracking for LRU eviction.
	_ = c.db.UpdateLastAccess(rel, time.Now().Unix())
	return n, err
}

// Write writes data to the cache file at offset, marks the entry dirty, and
// queues a put operation.
func (c *CacheLayer) Write(rel string, data []byte, off int64) (int, error) {
	dp := c.diskPath(rel)

	f, err := os.OpenFile(dp, os.O_WRONLY, 0)
	if err != nil {
		return 0, err
	}
	n, writeErr := f.WriteAt(data, off)
	f.Close()
	if writeErr != nil {
		return n, writeErr
	}

	// Update metadata.
	now := time.Now().Unix()

	var st syscall.Stat_t
	if err := syscall.Lstat(dp, &st); err != nil {
		return n, err
	}

	tx, err := c.db.BeginTx()
	if err != nil {
		return n, err
	}
	defer tx.Rollback()

	if err := c.db.PutFileTx(tx, &FileEntry{
		Path:       rel,
		Size:       st.Size,
		Mode:       st.Mode,
		LocalMtime: now,
		CachePath:  dp,
		State:      StateDirty,
	}); err != nil {
		return n, err
	}
	if err := c.db.AddPendingOpTx(tx, &PendingOp{
		Op:       "put",
		Path:     rel,
		QueuedAt: now,
	}); err != nil {
		return n, err
	}
	return n, tx.Commit()
}

// Create creates a new file in the cache, inserts a dirty DB entry, and
// queues a put operation. Returns the open file handle and the FileEntry.
func (c *CacheLayer) Create(rel string, mode uint32) (*os.File, *FileEntry, error) {
	dp := c.diskPath(rel)

	// Ensure parent directory exists on disk.
	if err := os.MkdirAll(filepath.Dir(dp), 0755); err != nil {
		return nil, nil, err
	}

	f, err := os.OpenFile(dp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, os.FileMode(mode))
	if err != nil {
		return nil, nil, err
	}

	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		f.Close()
		return nil, nil, err
	}

	now := time.Now().Unix()
	entry := &FileEntry{
		Path:       rel,
		IsDir:      false,
		Size:       0,
		Mode:       st.Mode,
		LocalMtime: now,
		CachePath:  dp,
		State:      StateDirty,
	}

	tx, err := c.db.BeginTx()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	defer tx.Rollback()

	if err := c.db.PutFileTx(tx, entry); err != nil {
		f.Close()
		return nil, nil, err
	}
	if err := c.db.AddPendingOpTx(tx, &PendingOp{
		Op:       "put",
		Path:     rel,
		QueuedAt: now,
	}); err != nil {
		f.Close()
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, entry, nil
}

// Truncate truncates the cache file and updates the DB entry.
func (c *CacheLayer) Truncate(rel string, size int64) error {
	dp := c.diskPath(rel)
	if err := os.Truncate(dp, size); err != nil {
		return err
	}

	now := time.Now().Unix()
	tx, err := c.db.BeginTx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Read current entry to preserve fields.
	existing, err := c.db.GetFile(rel)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("truncate: path %q not in DB", rel)
	}
	existing.Size = size
	existing.LocalMtime = now
	existing.State = StateDirty

	if err := c.db.PutFileTx(tx, existing); err != nil {
		return err
	}
	if err := c.db.AddPendingOpTx(tx, &PendingOp{
		Op:       "put",
		Path:     rel,
		QueuedAt: now,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// Chmod changes permissions on the cache file and updates the mode in the DB.
func (c *CacheLayer) Chmod(rel string, mode os.FileMode) error {
	dp := c.diskPath(rel)
	if err := os.Chmod(dp, mode); err != nil {
		return err
	}

	// Re-stat to get the actual POSIX mode.
	var st syscall.Stat_t
	if err := syscall.Lstat(dp, &st); err != nil {
		return err
	}

	existing, err := c.db.GetFile(rel)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("chmod: path %q not in DB", rel)
	}
	existing.Mode = st.Mode
	return c.db.PutFile(existing)
}

// Chtimes changes the access and modification times on the cache file.
func (c *CacheLayer) Chtimes(rel string, atime, mtime time.Time) error {
	dp := c.diskPath(rel)
	if err := os.Chtimes(dp, atime, mtime); err != nil {
		return err
	}

	existing, err := c.db.GetFile(rel)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("chtimes: path %q not in DB", rel)
	}
	existing.LocalMtime = mtime.Unix()
	return c.db.PutFile(existing)
}

// Delete removes the cache file, marks the entry deleted_local, and queues
// a delete operation.
func (c *CacheLayer) Delete(rel string) error {
	dp := c.diskPath(rel)
	if err := os.Remove(dp); err != nil && !os.IsNotExist(err) {
		return err
	}

	now := time.Now().Unix()
	tx, err := c.db.BeginTx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Instead of keeping the entry as deleted_local, we remove it from the DB
	// for now (no remote to sync with yet). This matches the FUSE semantics
	// where a deleted file no longer exists.
	if _, err := tx.Exec(`DELETE FROM files WHERE path = ?`, rel); err != nil {
		return err
	}
	if err := c.db.AddPendingOpTx(tx, &PendingOp{
		Op:       "delete",
		Path:     rel,
		QueuedAt: now,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// Mkdir creates a directory in the cache and inserts a dirty DB entry.
func (c *CacheLayer) Mkdir(rel string, mode uint32) (*FileEntry, error) {
	dp := c.diskPath(rel)
	if err := os.Mkdir(dp, os.FileMode(mode)); err != nil {
		return nil, err
	}

	var st syscall.Stat_t
	if err := syscall.Lstat(dp, &st); err != nil {
		return nil, err
	}

	now := time.Now().Unix()
	entry := &FileEntry{
		Path:       rel,
		IsDir:      true,
		Mode:       st.Mode,
		LocalMtime: now,
		CachePath:  dp,
		State:      StateDirty,
	}

	tx, err := c.db.BeginTx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if err := c.db.PutFileTx(tx, entry); err != nil {
		return nil, err
	}
	if err := c.db.AddPendingOpTx(tx, &PendingOp{
		Op:       "mkdir",
		Path:     rel,
		QueuedAt: now,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return entry, nil
}

// Rmdir removes a directory from the cache and queues a rmdir operation.
func (c *CacheLayer) Rmdir(rel string) error {
	dp := c.diskPath(rel)
	if err := os.Remove(dp); err != nil {
		return err
	}

	now := time.Now().Unix()
	tx, err := c.db.BeginTx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM files WHERE path = ?`, rel); err != nil {
		return err
	}
	if err := c.db.AddPendingOpTx(tx, &PendingOp{
		Op:       "rmdir",
		Path:     rel,
		QueuedAt: now,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// Rename renames a file/dir in the cache, updates the DB path, and queues
// a rename operation.
func (c *CacheLayer) Rename(oldRel, newRel string) error {
	oldDP := c.diskPath(oldRel)
	newDP := c.diskPath(newRel)

	// Ensure destination parent exists on disk.
	if err := os.MkdirAll(filepath.Dir(newDP), 0755); err != nil {
		return err
	}

	if err := os.Rename(oldDP, newDP); err != nil {
		return err
	}

	now := time.Now().Unix()
	tx, err := c.db.BeginTx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Read old entry, delete it, insert under new path.
	existing, err := c.db.GetFile(oldRel)
	if err != nil {
		return err
	}
	if existing != nil {
		if _, err := tx.Exec(`DELETE FROM files WHERE path = ?`, oldRel); err != nil {
			return err
		}
		existing.Path = newRel
		existing.CachePath = newDP
		existing.LocalMtime = now
		if err := c.db.PutFileTx(tx, existing); err != nil {
			return err
		}
	}

	// Update any queued pending_ops that reference the old path so that the
	// sync engine operates on the correct (new) name. Both updates share the
	// same transaction as the files-table update → atomic rename.
	if _, err := tx.Exec(
		`UPDATE pending_ops SET path = ? WHERE op IN ('put','mkdir') AND path = ?`,
		newRel, oldRel,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE pending_ops SET dest_path = ? WHERE op = 'rename' AND dest_path = ?`,
		newRel, oldRel,
	); err != nil {
		return err
	}

	if err := c.db.AddPendingOpTx(tx, &PendingOp{
		Op:       "rename",
		Path:     oldRel,
		DestPath: newRel,
		QueuedAt: now,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------- Seed helper ----------

// SeedFromDir walks srcDir and populates the cache from it.
// Every file/dir gets a "clean" state entry. This is a transitional helper
// for Phase 1 compatibility — Phase 3 replaces it with remote population.
func (c *CacheLayer) SeedFromDir(srcDir string) error {
	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil // skip root
		}

		dp := c.diskPath(rel)

		if d.IsDir() {
			if err := os.MkdirAll(dp, 0755); err != nil {
				return err
			}
			var st syscall.Stat_t
			if err := syscall.Lstat(dp, &st); err != nil {
				return err
			}
			return c.db.PutFile(&FileEntry{
				Path:       rel,
				IsDir:      true,
				Mode:       st.Mode,
				LocalMtime: time.Now().Unix(),
				CachePath:  dp,
				State:      StateClean,
			})
		}

		// Regular file: copy contents.
		if err := os.MkdirAll(filepath.Dir(dp), 0755); err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if err := os.WriteFile(dp, data, info.Mode().Perm()); err != nil {
			return err
		}
		var st syscall.Stat_t
		if err := syscall.Lstat(dp, &st); err != nil {
			return err
		}
		return c.db.PutFile(&FileEntry{
			Path:       rel,
			IsDir:      false,
			Size:       st.Size,
			Mode:       st.Mode,
			LocalMtime: time.Now().Unix(),
			CachePath:  dp,
			State:      StateClean,
		})
	})
}

// LstatDisk runs syscall.Lstat on the cache file at rel and returns the Stat_t.
// This is used by the FUSE layer to get full POSIX attributes from disk
// as a supplement to DB metadata.
func (c *CacheLayer) LstatDisk(rel string) (*syscall.Stat_t, error) {
	var st syscall.Stat_t
	if err := syscall.Lstat(c.diskPath(rel), &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// FstatDisk runs syscall.Fstat on an open file descriptor.
func (c *CacheLayer) FstatDisk(fd int) (*syscall.Stat_t, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// OpenOrCreate opens an existing cache file if it already has the expected
// size (preserving partially-downloaded data for resume), or creates a new
// sparse file truncated to size.
func (c *CacheLayer) OpenOrCreate(rel string, size int64) (*os.File, error) {
	dp := c.diskPath(rel)
	if err := os.MkdirAll(filepath.Dir(dp), 0755); err != nil {
		return nil, err
	}

	// Try to reuse existing file if it has the right size (resume).
	if info, err := os.Stat(dp); err == nil && info.Size() == size {
		f, err := os.OpenFile(dp, os.O_RDWR, 0644)
		if err == nil {
			return f, nil
		}
	}

	f, err := os.OpenFile(dp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return nil, err
	}
	if size > 0 {
		if err := f.Truncate(size); err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}

// DiskPath returns the absolute path on disk for a relative cache path.
// Exported for use by the download manager.
func (c *CacheLayer) DiskPath(rel string) string {
	return c.diskPath(rel)
}

// FilesDir returns the absolute path to the files/ subdirectory of the cache.
// Used by the evictor to compute total disk usage.
func (c *CacheLayer) FilesDir() string {
	return c.filesDir
}
