# Phase 8 — Hardening + Edge Cases

> **Goal:** Handle real-world edge cases and production hardening. Make the system resilient to crashes, corruption, and concurrent access.

---

## Key Steps

### 1. Large file streaming

- Verify Download Manager handles files larger than available RAM — chunks are written to disk incrementally, never buffered fully in memory
- Verify uploads stream from cache file via `io.Reader` without loading the entire file into memory
- Test with files >10 GB: download, upload, and concurrent read/write

### 2. Download resume after crash

- On mount startup, scan `files WHERE state = 'downloading'` — these represent downloads interrupted by a crash
- Read persisted `cached_ranges` from DB — these bytes are already on disk in the cache file
- For backends with `SupportsRange() == true`: call `Gaps(totalSize)` on the range set, issue `GetRange` for each gap → resume without re-downloading completed intervals
- For backends without range support: reset to `evicted` and restart from offset 0 on next access
- Add startup recovery logic to `internal/download/manager.go`

### 3. Rename while dirty

- When a dirty file is renamed via FUSE:
  - Update `path` in the `files` table
  - Update `path` (and possibly `dest_path`) in `pending_ops` entries that reference the old path
  - Both updates must be in a single SQLite transaction to maintain consistency
- If the sync engine is currently uploading the old path, let it finish, then handle the rename as a separate `rename` op

### 4. Delete while syncing or downloading

- **Delete during sync:** cancel the in-flight `Put` call (via context), mark `deleted_local`, queue a `delete` op
- **Delete during download:** cancel the download goroutine (via context), remove cache file, mark `deleted_local`, queue a `delete` op
- Ensure no dangling goroutines or temporary files remain

### 5. Concurrent writes to same file

- Serialize writes to the same path using per-file mutexes in the FUSE layer
- If two processes write to the same file, the second write waits for the first to complete its cache file write + DB update
- Advisory locking on cache files to prevent corruption from concurrent processes

### 6. Cache corruption recovery

- On read, optionally verify checksum of cache file against stored `checksum` in DB
- If mismatch detected: log warning, set state to `evicted`, trigger re-download from remote on next access
- Checksum computation: run after download completes or after sync marks file clean
- Configurable: `--verify-checksums` flag (off by default for performance)

### 7. Mount persistence

- `rvfs mount --install-service` — generate and install:
  - **Linux:** systemd user unit file (`~/.config/systemd/user/rvfs-<name>.service`)
  - **macOS:** launchd plist (`~/Library/LaunchAgents/com.rvfs.<name>.plist`)
- Service auto-starts on login, runs `rvfs mount` with saved config
- `rvfs mount --uninstall-service` — remove the generated service file

### 8. Foreground mode + logging

- `rvfs mount --foreground` — keep process in foreground (don't daemonize)
- Structured logging to stderr with configurable level (`--log-level debug|info|warn|error`)
- Log sync events, connectivity transitions, download progress, and errors

---

## Files to Create / Modify

| File                           | Action | Purpose                                                         |
| ------------------------------ | ------ | --------------------------------------------------------------- |
| `internal/download/manager.go` | Modify | Crash resume, large file verification                           |
| `internal/cache/cache.go`      | Modify | Checksum verification, corruption recovery                      |
| `internal/cache/db.go`         | Modify | Atomic rename/delete transactions                               |
| `internal/fuse/node.go`        | Modify | Per-file write serialization                                    |
| `internal/cli/mount.go`        | Modify | `--foreground`, `--verify-checksums`, `--install-service` flags |
| `internal/service/systemd.go`  | Create | systemd unit generation                                         |
| `internal/service/launchd.go`  | Create | launchd plist generation                                        |

---

## Exit Criteria

- [ ] Stream a 10 GB+ file without OOM — memory stays bounded
- [ ] Kill process mid-download, restart mount — download resumes from persisted `cached_ranges` (range-capable backends)
- [ ] Rename a dirty file → `pending_ops` updated atomically, sync succeeds with correct path
- [ ] Delete a file mid-sync → upload cancelled cleanly, `deleted_local` state set, no goroutine leaks
- [ ] Delete a file mid-download → download cancelled, cache cleaned up, no goroutine leaks
- [ ] Two processes writing the same file concurrently → no corruption, writes serialized
- [ ] Corrupt a cache file manually → next read triggers re-download (with `--verify-checksums`)
- [ ] `rvfs mount --install-service` generates a working systemd unit / launchd plist
- [ ] `rvfs mount --foreground --log-level debug` shows detailed structured logs
