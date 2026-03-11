# Phase 6 — CLI Polish + Cache Eviction

> **Goal:** Production-ready CLI with rich terminal output. Cache eviction keeps disk usage bounded. Full remote/config management.

---

## Dependencies

| Package                             | Purpose                                     |
| ----------------------------------- | ------------------------------------------- |
| `github.com/charmbracelet/lipgloss` | Terminal formatting for status/queue output |

---

## Key Steps

### 1. Remote management

Modify `internal/cli/remote.go`:

- `rvfs remote add <type> <name>` — interactive prompts for backend-specific config, stores in `~/.config/rvfs/config.toml`
- `rvfs remotes` — list all configured remotes with type and status

### 2. Config file management

Create `internal/config/config.go`:

- Load/save `~/.config/rvfs/config.toml`
- Per-remote config sections: type, credentials path, endpoint, etc.
- Per-mount config: cache-dir, cache-size, poll-interval, probe-interval, conflict strategy

### 3. `status` command

Create `internal/cli/status.go`:

- `rvfs status` — all active mounts
- `rvfs status <mountpoint>` — specific mount
- Output (formatted with lipgloss):
  ```
  Mount:     ~/Documents
  Remote:    gdrive:Documents
  State:     ONLINE
  Cache:     4.2 GB / 20.0 GB (21%)
  Pending:   3 files to upload
  Conflicts: 1 unresolved
  ```

### 4. `queue` command

Create `internal/cli/queue.go`:

- `rvfs queue [mountpoint]` — show pending sync operations from `pending_ops` table
- Output:
  ```
  PRIORITY  PATH                     OP      SIZE    QUEUED
  1         /docs/report.pdf         upload  2.1 MB  5 minutes ago
  2         /docs/notes.txt          upload  4 KB    5 minutes ago
  3         /old-file.txt            delete  —       8 minutes ago
  ```

### 5. `sync` command

Create `internal/cli/sync.go`:

- `rvfs sync [mountpoint]` — trigger immediate sync cycle
- `rvfs sync [mountpoint] --force` — clear all `retry_after` timestamps first, then sync

### 6. `umount` command

Create `internal/cli/umount.go`:

- `rvfs umount <mountpoint>` — graceful unmount: flush pending writes warning, unmount FUSE

### 7. `pin` / `unpin` / `pins` commands

Create `internal/cli/pin.go`:

- `rvfs pin <path>` — mark path as pinned (never evict)
- `rvfs unpin <path>` — remove pin
- `rvfs pins [mountpoint]` — list pinned paths

Add a `pinned` boolean column to the `files` table (or a separate `pins` table).

### 8. LRU cache eviction

Create `internal/cache/eviction.go`:

- Track last access time per file (update on `Read` / `Open`)
- When total cache size exceeds `--cache-size` (default 20G):
  - Query `files WHERE state = 'clean' AND pinned = 0 ORDER BY last_access ASC`
  - Delete cache files until usage drops below threshold
  - Set evicted files' state to `evicted`
- Support `--cache-max-age` — evict clean files older than this duration regardless of space
- Run eviction check periodically (e.g., after every sync cycle or on a timer)

---

## Files to Create / Modify

| File                         | Action | Purpose                                         |
| ---------------------------- | ------ | ----------------------------------------------- |
| `internal/config/config.go`  | Create | Config file load/save                           |
| `internal/cli/status.go`     | Create | `status` command                                |
| `internal/cli/queue.go`      | Create | `queue` command                                 |
| `internal/cli/sync.go`       | Create | `sync` command                                  |
| `internal/cli/umount.go`     | Create | `umount` command                                |
| `internal/cli/pin.go`        | Create | `pin`, `unpin`, `pins` commands                 |
| `internal/cli/remote.go`     | Modify | Full `remote add`/`remotes` with config prompts |
| `internal/cache/eviction.go` | Create | LRU eviction logic                              |
| `internal/cache/db.go`       | Modify | Add `pinned`/`last_access` columns              |
| `internal/cli/mount.go`      | Modify | Add `--cache-size`, `--cache-max-age` flags     |

---

## Exit Criteria

- [ ] All CLI subcommands listed in OVERVIEW §8 are implemented and produce formatted output
- [ ] `rvfs status` shows mount info, cache usage %, pending count, conflicts count
- [ ] `rvfs queue` shows pending operations with sizes and timestamps
- [ ] `rvfs sync --force` clears backoff timers and retries immediately
- [ ] `rvfs pin`/`unpin`/`pins` correctly manage pinned paths
- [ ] Cache eviction triggers when `--cache-size` is exceeded; only clean, unpinned files evicted
- [ ] `--cache-max-age` evicts stale clean files regardless of space pressure
- [ ] Config file persists remote credentials and per-mount settings across restarts
