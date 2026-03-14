package cache

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Evictor enforces a maximum cache size and optional per-file age limit by
// removing clean, unpinned files from the local cache. Evicted entries have
// their state set to StateEvicted so FUSE knows to re-download on next access.
type Evictor struct {
	// MaxSize is the maximum total size of the files/ directory in bytes.
	// 0 means no size limit.
	MaxSize int64
	// MaxAge is the maximum time since a clean file was last accessed before
	// it is evicted regardless of space pressure. 0 means no age limit.
	MaxAge time.Duration
	// MinFreeSpace is the minimum free space that must remain on the
	// filesystem containing the cache directory. Clean unpinned files are
	// evicted (LRU-first) until the threshold is satisfied. 0 = disabled.
	MinFreeSpace int64

	// TriggerC is an optional channel the sync engine uses to nudge the
	// evictor after every sync cycle. The evictor buffers and de-duplicates
	// triggers so the channel should be unbuffered or buffered≥1.
	TriggerC <-chan struct{}
}

// Run starts the eviction loop. It blocks until ctx is cancelled.
// A fallback ticker fires every 5 minutes regardless of TriggerC.
func (ev *Evictor) Run(ctx context.Context, cl *CacheLayer) {
	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()

	ev.check(cl)

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			ev.check(cl)
		case _, ok := <-ev.TriggerC:
			if !ok {
				return
			}
			ev.check(cl)
		}
	}
}

// check runs a single eviction pass: first by age, then by
// minimum free space, then by size.
func (ev *Evictor) check(cl *CacheLayer) {
	if ev.MaxAge > 0 {
		ev.evictByAge(cl)
	}
	if ev.MinFreeSpace > 0 {
		ev.evictByFreeSpace(cl)
	}
	if ev.MaxSize > 0 {
		ev.evictBySize(cl)
	}
}

// evictByAge removes clean unpinned files not accessed within MaxAge.
func (ev *Evictor) evictByAge(cl *CacheLayer) {
	entries, err := cl.db.ListEvictable()
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-ev.MaxAge).Unix()
	for _, e := range entries {
		if e.LastAccess > cutoff {
			continue
		}
		evictEntry(cl, e)
	}
}

// evictByFreeSpace removes the least-recently-accessed clean unpinned files
// until the free space on the cache filesystem reaches MinFreeSpace.
func (ev *Evictor) evictByFreeSpace(cl *CacheLayer) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(cl.filesDir, &st); err != nil {
		return
	}
	// Available bytes for unprivileged processes.
	avail := int64(st.Bavail) * st.Bsize
	if avail >= ev.MinFreeSpace {
		return
	}

	entries, err := cl.db.ListEvictable()
	if err != nil {
		return
	}

	for _, e := range entries {
		if avail >= ev.MinFreeSpace {
			break
		}
		freed := e.Size
		evictEntry(cl, e)
		avail += freed
	}
}

// evictBySize removes the least-recently-accessed clean unpinned files until
// the total files/ directory usage is below MaxSize.
func (ev *Evictor) evictBySize(cl *CacheLayer) {
	used, err := dirSize(cl.filesDir)
	if err != nil || used <= ev.MaxSize {
		return
	}

	entries, err := cl.db.ListEvictable()
	if err != nil {
		return
	}

	for _, e := range entries {
		if used <= ev.MaxSize {
			break
		}
		freed := e.Size
		evictEntry(cl, e)
		used -= freed
	}
}

// evictEntry removes the on-disk cache file and marks the DB entry evicted.
func evictEntry(cl *CacheLayer, e *FileEntry) {
	dp := cl.diskPath(e.Path)
	if err := os.Remove(dp); err != nil && !os.IsNotExist(err) {
		return
	}
	if err := cl.db.SetState(e.Path, StateEvicted); err != nil {
		slog.Warn("evict: set state failed", "path", e.Path, "err", err)
	}
}

// EvictPath removes one cached file and marks its DB state as StateEvicted.
// Missing on-disk files are tolerated. Directories are not evictable.
func EvictPath(cl *CacheLayer, path string) error {
	e, err := cl.db.GetFile(path)
	if err != nil {
		return fmt.Errorf("evict path %q: %w", path, err)
	}
	if e == nil {
		return fmt.Errorf("evict path: %q not found", path)
	}
	if e.IsDir {
		return fmt.Errorf("evict path: %q is a directory", path)
	}
	evictEntry(cl, e)
	return nil
}

// UsageStats summarizes logical and physical byte usage for cache files.
// LogicalBytes is the sum of file lengths. PhysicalBytes is allocated blocks.
type UsageStats struct {
	LogicalBytes  int64
	PhysicalBytes int64
}

// dirSize returns the total byte size of all regular files under dir.
func dirSize(dir string) (int64, error) {
	usage, err := dirUsage(dir)
	if err != nil {
		return 0, err
	}
	return usage.LogicalBytes, nil
}

// dirUsage returns logical and physical byte usage for all regular files under dir.
func dirUsage(dir string) (UsageStats, error) {
	var usage UsageStats
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return nil // skip unreadable files
		}
		logical := info.Size()
		usage.LogicalBytes += logical
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			usage.PhysicalBytes += st.Blocks * 512
			return nil
		}

		// Fallback for platforms/filesystems where block allocation isn't exposed.
		usage.PhysicalBytes += logical
		return nil
	})
	return usage, err
}

// DirSize is the exported version of dirSize, used by callers outside the
// cache package (e.g. the IPC status handler in the CLI).
func DirSize(dir string) (int64, error) {
	return dirSize(dir)
}

// DirUsage returns both logical and physical byte usage under dir.
func DirUsage(dir string) (UsageStats, error) {
	return dirUsage(dir)
}
