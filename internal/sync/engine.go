package sync

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/connectivity"
	"github.com/IstarVin/rvfs/internal/remote"
)

// Engine is the background sync engine that uploads dirty files and pulls
// remote changes on a configurable interval.
type Engine struct {
	adapter  remote.RemoteAdapter
	cache    *cache.CacheLayer
	interval time.Duration
	stopCh   chan struct{}
	doneCh   chan struct{}

	// Connectivity-aware pause/resume. monitor may be nil (local / no monitor).
	monitor       *connectivity.Monitor
	paused        atomic.Bool
	connWatchDone chan struct{} // closed when watchConn exits; nil if no monitor

	// resolver applies the configured conflict strategy. nil when no adapter.
	resolver *Resolver

	// uploadCtxs tracks cancellation functions for uploads in flight.
	// Keyed by file path; used by CancelUpload to abort an ongoing Put.
	uploadMu   sync.Mutex
	uploadCtxs map[string]context.CancelFunc
}

// NewEngine creates a sync engine. monitor may be nil for local mounts or
// when no connectivity tracking is desired. strategy controls conflict
// resolution behaviour (see ConflictStrategy constants); if empty, StrategyBoth
// is used.
func NewEngine(adapter remote.RemoteAdapter, cl *cache.CacheLayer, interval time.Duration, monitor *connectivity.Monitor, strategy ConflictStrategy) *Engine {
	if strategy == "" {
		strategy = StrategyBoth
	}
	e := &Engine{
		adapter:    adapter,
		cache:      cl,
		interval:   interval,
		monitor:    monitor,
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
		uploadCtxs: make(map[string]context.CancelFunc),
	}
	if adapter != nil {
		e.resolver = NewResolver(strategy, cl, adapter)
	}
	return e
}

// Start launches the background sync loop (and the connectivity watcher if a
// monitor was provided).
func (e *Engine) Start() {
	if e.monitor != nil {
		e.connWatchDone = make(chan struct{})
		go e.watchConn()
	}
	go e.loop()
}

// Stop signals the background goroutine to stop and waits for it to exit.
func (e *Engine) Stop() {
	close(e.stopCh)
	<-e.doneCh
	if e.connWatchDone != nil {
		<-e.connWatchDone
	}
}

func (e *Engine) loop() {
	defer close(e.doneCh)

	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()

	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			if e.paused.Load() {
				continue
			}
			e.uploadDirty()
			e.processPendingOps()
			e.pull()
		}
	}
}

// watchConn subscribes to monitor state transitions and pauses/resumes the
// sync loop accordingly. On RECONNECTING it drains the dirty queue before
// calling NotifyQueueDrained so the monitor can transition to ONLINE.
func (e *Engine) watchConn() {
	defer close(e.connWatchDone)

	sub := e.monitor.Subscribe()
	for {
		select {
		case <-e.stopCh:
			return
		case s, ok := <-sub:
			if !ok {
				return
			}
			switch s {
			case connectivity.StateOffline:
				e.paused.Store(true)
			case connectivity.StateReconnecting:
				// Drain dirty queue while still paused, then let the monitor know
				// and resume normal polling.
				e.uploadDirty()
				e.processPendingOps()
				e.monitor.NotifyQueueDrained()
				e.paused.Store(false)
			}
		}
	}
}

// PullOnce runs a single pull cycle synchronously. Used for initial mount
// population before the background loop starts.
func (e *Engine) PullOnce() error {
	return e.pull()
}

// CancelUpload aborts any in-flight upload for the given path. It is a
// no-op if no upload is currently active for that path.
func (e *Engine) CancelUpload(path string) {
	e.uploadMu.Lock()
	cancel, ok := e.uploadCtxs[path]
	e.uploadMu.Unlock()
	if ok {
		cancel()
	}
}

// monitorCtx returns the connectivity monitor's context when a monitor is
// configured, or context.Background() otherwise. Using this as the parent
// for upload contexts means uploads are automatically cancelled when the
// monitor transitions to OFFLINE.
func (e *Engine) monitorCtx() context.Context {
	if e.monitor != nil {
		return e.monitor.Context()
	}
	return context.Background()
}

// ---------- Upload ----------

func (e *Engine) uploadDirty() {
	entries, err := e.cache.DB().ListByState(cache.StateDirty)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir {
			continue
		}
		e.uploadFile(entry)
	}
}

func (e *Engine) uploadFile(entry *cache.FileEntry) {
	// Before uploading check whether the remote has been modified since our
	// last sync point. If so, delegate to the conflict resolver instead of
	// overwriting the remote change. Only do this for files that have a known
	// remote mtime (i.e. were previously synced); brand-new local files
	// (RemoteMtime==0) are always safe to upload.
	if e.resolver != nil && entry.RemoteMtime > 0 {
		statCtx, statCancel := context.WithTimeout(context.Background(), 30*time.Second)
		remoteStat, statErr := e.adapter.Stat(statCtx, entry.Path)
		statCancel()
		if statErr == nil && remoteStat.Mtime.Unix() > entry.RemoteMtime {
			if err := e.resolver.Resolve(entry, remoteStat); err != nil {
				slog.Warn("sync: conflict resolve failed", "path", entry.Path, "err", err)
			}
			return
		}
	}

	// Set state to syncing.
	if err := e.cache.DB().SetState(entry.Path, cache.StateSyncing); err != nil {
		return
	}

	f, err := e.cache.Open(entry.Path, os.O_RDONLY)
	if err != nil {
		if dbErr := e.cache.DB().SetState(entry.Path, cache.StateDirty); dbErr != nil {
			slog.Warn("sync: revert to dirty failed", "path", entry.Path, "err", dbErr)
		}
		return
	}
	defer f.Close()

	// Register a cancellable context so Unlink can abort this upload.
	// Use monitorCtx() as parent so uploads auto-cancel on OFFLINE.
	ctx, cancel := context.WithCancel(e.monitorCtx())
	e.uploadMu.Lock()
	e.uploadCtxs[entry.Path] = cancel
	e.uploadMu.Unlock()
	defer func() {
		e.uploadMu.Lock()
		delete(e.uploadCtxs, entry.Path)
		e.uploadMu.Unlock()
		cancel()
	}()

	mtime := time.Unix(entry.LocalMtime, 0)
	if err := e.adapter.Put(ctx, entry.Path, f, entry.Size, mtime); err != nil {
		// Revert to dirty on failure.
		if dbErr := e.cache.DB().SetState(entry.Path, cache.StateDirty); dbErr != nil {
			slog.Warn("sync: revert to dirty failed", "path", entry.Path, "err", dbErr)
		}
		// Record sync error.
		entry.SyncError = err.Error()
		entry.State = cache.StateDirty
		if dbErr := e.cache.DB().PutFile(entry); dbErr != nil {
			slog.Warn("sync: record sync error failed", "path", entry.Path, "err", dbErr)
		}
		return
	}

	// Success — mark clean.
	entry.State = cache.StateClean
	entry.RemoteMtime = entry.LocalMtime
	entry.SyncError = ""
	if err := e.cache.DB().PutFile(entry); err != nil {
		slog.Warn("sync: mark clean failed", "path", entry.Path, "err", err)
	}
}

// ---------- Pending Ops ----------

func (e *Engine) processPendingOps() {
	ops, err := e.cache.DB().NextPendingOps(10)
	if err != nil {
		return
	}

	for _, op := range ops {
		opCtx, opCancel := context.WithTimeout(context.Background(), 30*time.Second)
		var opErr error
		switch op.Op {
		case "delete":
			opErr = e.adapter.Delete(opCtx, op.Path)
		case "mkdir":
			opErr = e.adapter.Mkdir(opCtx, op.Path)
		case "rmdir":
			opErr = e.adapter.Delete(opCtx, op.Path) // Drive doesn't distinguish
		case "rename":
			opErr = e.adapter.Rename(opCtx, op.Path, op.DestPath)
		case "put":
			// Handled by uploadDirty; skip here to avoid double upload.
			opErr = nil
		}
		opCancel()

		if opErr != nil {
			// Increment attempt count; leave for retry.
			op.Attempts++
			op.LastError = opErr.Error()
			slog.Warn("sync: pending op failed", "op", op.Op, "path", op.Path, "err", opErr)
			continue
		}

		if err := e.cache.DB().CompletePendingOp(op.ID); err != nil {
			slog.Warn("sync: complete pending op failed", "id", op.ID, "err", err)
		}
	}
}

// ---------- Pull ----------

func (e *Engine) pull() error {
	return e.pullDir("")
}

func (e *Engine) pullDir(dirPath string) error {
	listCtx, listCancel := context.WithTimeout(context.Background(), 30*time.Second)
	remoteFiles, err := e.adapter.List(listCtx, dirPath)
	listCancel()
	if err != nil {
		return fmt.Errorf("pull list %q: %w", dirPath, err)
	}

	// Build a set of remote paths we've seen for deletion detection.
	remotePaths := make(map[string]struct{})

	for _, rf := range remoteFiles {
		remotePaths[rf.Path] = struct{}{}

		existing, err := e.cache.DB().GetFile(rf.Path)
		if err != nil {
			continue
		}

		remoteMtime := rf.Mtime.Unix()

		if existing == nil {
			// New remote file — add to DB as evicted (metadata only).
			mode := uint32(0644)
			if rf.IsDir {
				mode = 0040755 // S_IFDIR | 0755
			} else {
				mode = 0100644 // S_IFREG | 0644
			}
			if err := e.cache.DB().PutFile(&cache.FileEntry{
				Path:        rf.Path,
				IsDir:       rf.IsDir,
				Size:        rf.Size,
				Mode:        mode,
				RemoteMtime: remoteMtime,
				LocalMtime:  remoteMtime,
				CachePath:   e.cache.DiskPath(rf.Path),
				State:       cache.StateEvicted,
				Checksum:    rf.Checksum,
			}); err != nil {
				slog.Warn("sync: insert new remote file failed", "path", rf.Path, "err", err)
			}
			continue
		}

		// Check if remote is newer.
		if remoteMtime > existing.RemoteMtime {
			switch existing.State {
			case cache.StateClean, cache.StateEvicted:
				// Remote is newer — re-evict, delete local cache.
				existing.State = cache.StateEvicted
				existing.Size = rf.Size
				existing.RemoteMtime = remoteMtime
				existing.Checksum = rf.Checksum
				if err := e.cache.DB().PutFile(existing); err != nil {
					slog.Warn("sync: re-evict on remote update failed", "path", rf.Path, "err", err)
				}
				// Remove stale cache file if it exists.
				os.Remove(e.cache.DiskPath(rf.Path))

			case cache.StateDirty, cache.StateSyncing:
				// Conflict: remote has changed while we have local edits.
				// Delegate to the resolver (strategy = both/local-wins/…).
				if e.resolver != nil {
					existing.RemoteMtime = remoteMtime
					if err := e.resolver.Resolve(existing, rf); err != nil {
						slog.Warn("sync: conflict resolve failed", "path", rf.Path, "err", err)
					}
				} else {
					existing.State = cache.StateConflict
					existing.RemoteMtime = remoteMtime
					if err := e.cache.DB().PutFile(existing); err != nil {
						slog.Warn("sync: mark conflict failed", "path", rf.Path, "err", err)
					}
				}
			}
		}
	}

	// Detect remote deletions: check local entries that are no longer remote.
	localEntries, err := e.cache.DB().ListDir(dirPath)
	if err != nil {
		return nil
	}
	for _, le := range localEntries {
		if _, found := remotePaths[le.Path]; !found {
			if le.State == cache.StateClean || le.State == cache.StateEvicted {
				if err := e.cache.DB().DeleteFile(le.Path); err != nil {
					slog.Warn("sync: delete stale local entry failed", "path", le.Path, "err", err)
				}
				os.Remove(e.cache.DiskPath(le.Path))
			}
		}
	}

	// Recursively sync subdirectories.
	for _, rf := range remoteFiles {
		if rf.IsDir {
			if err := e.pullDir(rf.Path); err != nil {
				// Log error but continue syncing other subdirectories.
				slog.Warn("sync: pull subdir failed", "path", rf.Path, "err", err)
			}
		}
	}

	return nil
}
