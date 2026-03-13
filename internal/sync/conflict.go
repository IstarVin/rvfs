package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/remote"
)

// ConflictStrategy determines how the sync engine behaves when a file has
// been modified both locally and on the remote since the last sync point.
type ConflictStrategy string

const (
	// StrategyBoth keeps both versions. The remote copy is downloaded
	// alongside the local file with a ".conflict.<timestamp>" suffix.
	// This is the default and guarantees zero data loss.
	StrategyBoth ConflictStrategy = "both"

	// StrategyLocalWins immediately re-queues the local copy for upload,
	// overwriting the remote change.
	StrategyLocalWins ConflictStrategy = "local-wins"

	// StrategyRemoteWins downloads the remote version and overwrites the
	// local cache, discarding local changes.
	StrategyRemoteWins ConflictStrategy = "remote-wins"

	// StrategyManual records the conflict and blocks sync for the file
	// entirely until the user resolves it via 'rvfs resolve'.
	StrategyManual ConflictStrategy = "manual"
)

// ValidConflictStrategy reports whether s is one of the four known strategies.
func ValidConflictStrategy(s string) bool {
	switch ConflictStrategy(s) {
	case StrategyBoth, StrategyLocalWins, StrategyRemoteWins, StrategyManual:
		return true
	}
	return false
}

// Resolver applies a ConflictStrategy when the sync engine detects that a
// file has been modified both locally and remotely.
type Resolver struct {
	strategy ConflictStrategy
	cl       *cache.CacheLayer
	adapter  remote.RemoteAdapter
}

// NewResolver creates a Resolver with the given strategy.
func NewResolver(strategy ConflictStrategy, cl *cache.CacheLayer, adapter remote.RemoteAdapter) *Resolver {
	return &Resolver{strategy: strategy, cl: cl, adapter: adapter}
}

// Resolve applies the configured strategy to entry.
// remoteStat must reflect the current remote state of entry.Path.
// entry.RemoteMtime should already be updated to remoteStat.Mtime.Unix()
// by the caller before invoking Resolve.
func (r *Resolver) Resolve(entry *cache.FileEntry, remoteStat remote.FileInfo) error {
	switch r.strategy {
	case StrategyLocalWins:
		return r.applyLocalWins(entry)
	case StrategyRemoteWins:
		return r.applyRemoteWins(entry, remoteStat)
	case StrategyManual:
		return r.applyManual(entry, remoteStat.Mtime.Unix())
	default: // StrategyBoth
		return r.applyBoth(entry, remoteStat)
	}
}

// applyLocalWins re-queues the local copy for upload. No conflict record is
// created because the conflict is considered immediately resolved.
func (r *Resolver) applyLocalWins(entry *cache.FileEntry) error {
	entry.State = cache.StateDirty
	entry.SyncError = ""
	return r.cl.DB().PutFile(entry)
}

// applyRemoteWins downloads the remote version and overwrites the local cache
// file, then marks the entry clean.
func (r *Resolver) applyRemoteWins(entry *cache.FileEntry, remoteStat remote.FileInfo) error {
	diskPath := r.cl.DiskPath(entry.Path)
	if err := os.MkdirAll(filepath.Dir(diskPath), 0755); err != nil {
		return fmt.Errorf("conflict remote-wins mkdir: %w", err)
	}

	f, err := os.Create(diskPath)
	if err != nil {
		return fmt.Errorf("conflict remote-wins create: %w", err)
	}

	if dlErr := r.adapter.Get(context.Background(), entry.Path, f); dlErr != nil {
		f.Close()
		os.Remove(diskPath)
		return fmt.Errorf("conflict remote-wins download: %w", dlErr)
	}
	f.Close()

	remoteMtime := remoteStat.Mtime.Unix()
	entry.State = cache.StateClean
	entry.RemoteMtime = remoteMtime
	entry.LocalMtime = remoteMtime
	entry.Size = remoteStat.Size
	entry.Checksum = remoteStat.Checksum
	entry.SyncError = ""
	return r.cl.DB().PutFile(entry)
}

// applyBoth keeps the local file as-is (marked conflict so uploads are
// skipped) and downloads the remote version to a ".conflict.<timestamp>"
// sidecar path.
func (r *Resolver) applyBoth(entry *cache.FileEntry, remoteStat remote.FileInfo) error {
	remoteMtime := remoteStat.Mtime.Unix()

	// Record the conflict for 'rvfs conflicts' to surface.
	if err := r.cl.DB().AddConflict(entry.Path, entry.LocalMtime, remoteMtime); err != nil {
		return fmt.Errorf("conflict both record: %w", err)
	}

	// Download the remote version to a sidecar path.
	conflictRel := fmt.Sprintf("%s.conflict.%d", entry.Path, remoteMtime)
	diskConflictPath := r.cl.DiskPath(conflictRel)
	if err := os.MkdirAll(filepath.Dir(diskConflictPath), 0755); err != nil {
		return fmt.Errorf("conflict both mkdir: %w", err)
	}

	f, err := os.Create(diskConflictPath)
	if err != nil {
		return fmt.Errorf("conflict both create: %w", err)
	}

	if dlErr := r.adapter.Get(context.Background(), entry.Path, f); dlErr != nil {
		// Download failed — still mark conflict so the user is notified.
		// The sidecar file is cleaned up.
		f.Close()
		os.Remove(diskConflictPath)
		entry.State = cache.StateConflict
		entry.RemoteMtime = remoteMtime
		_ = r.cl.DB().PutFile(entry)
		return nil
	}
	f.Close()

	// Register the sidecar file in the DB as a clean local-only file.
	_ = r.cl.DB().PutFile(&cache.FileEntry{
		Path:        conflictRel,
		IsDir:       false,
		Size:        remoteStat.Size,
		Mode:        entry.Mode,
		RemoteMtime: remoteMtime,
		LocalMtime:  remoteMtime,
		CachePath:   diskConflictPath,
		State:       cache.StateClean,
		Checksum:    remoteStat.Checksum,
	})

	// Mark the original entry as conflict so uploads are skipped.
	entry.State = cache.StateConflict
	entry.RemoteMtime = remoteMtime
	return r.cl.DB().PutFile(entry)
}

// applyManual records the conflict and sets state to conflict, blocking
// further sync until explicitly resolved via 'rvfs resolve'.
func (r *Resolver) applyManual(entry *cache.FileEntry, remoteMtime int64) error {
	if err := r.cl.DB().AddConflict(entry.Path, entry.LocalMtime, remoteMtime); err != nil {
		return fmt.Errorf("conflict manual record: %w", err)
	}
	entry.State = cache.StateConflict
	entry.RemoteMtime = remoteMtime
	return r.cl.DB().PutFile(entry)
}
