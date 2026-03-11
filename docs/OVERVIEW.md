# Offline-First FUSE Filesystem — Project Plan

> A CLI tool that mounts remote filesystems (S3, SFTP, etc.) locally with seamless offline support, automatic sync, and conflict resolution.

---

## Table of Contents

1. [Project Overview](#1-project-overview)
2. [Goals & Non-Goals](#2-goals--non-goals)
3. [Architecture](#3-architecture)
4. [Component Breakdown](#4-component-breakdown)
5. [Data Model](#5-data-model)
6. [Sync Engine Design](#6-sync-engine-design)
7. [Conflict Resolution](#7-conflict-resolution)
8. [CLI Interface](#8-cli-interface)
9. [Build Phases](#9-build-phases)
10. [Tech Stack](#10-tech-stack)
11. [Open Questions](#11-open-questions)

---

## 1. Project Overview

This tool mounts a remote filesystem (e.g. Google Drive, S3, SFTP) as a local directory using FUSE. Unlike rclone, the cache is the **primary storage layer** — the remote is treated as an async sync target. This means:

- All reads and writes always go through the local cache, regardless of connectivity
- The remote is synced to/from the cache in the background
- Going offline is not a special mode — the sync engine simply pauses

The result is a mountpoint that never returns errors due to connectivity loss.

---

## 2. Goals & Non-Goals

### Goals

- Mount a remote path as a local directory via FUSE
- Serve all reads from local cache (no remote calls on read path)
- Accept writes immediately into cache, sync to remote asynchronously
- Detect connectivity loss/restore automatically
- Pause sync on disconnect, resume on reconnect
- Track and surface write conflicts
- Support multiple remote backends (Google Drive initially)
- Provide a CLI for status, queue inspection, and conflict resolution

### Non-Goals (for v1)

- GUI or tray application
- Real-time remote change notifications (polling only in v1)
- Encryption at rest of the local cache
- Multi-user / shared cache scenarios
- Windows support (Linux + macOS only via FUSE)

---

## 3. Architecture

```
rvfs mount <remote>:<path> <mountpoint>
         │
         ├── FUSE Layer
         │       Implements: open, read, write, readdir, getattr,
         │                   create, unlink, mkdir, rmdir, rename
         │       All ops are dispatched to the Cache Layer only.
         │       Never calls remote directly.
         │
         ├── Cache Layer  (~/.cache/<appname>/<remote-id>/<path>)
         │       Actual files on disk — real inodes, real bytes.
         │       Metadata DB tracks state of every path.
         │       Dirty tracking: every write marks the file as dirty.
         │       Single source of truth for the mountpoint.
         │
         ├── Sync Engine  (background goroutine)
         │       Reads dirty entries from Metadata DB.
         │       Pushes dirty files → remote (upload).
         │       Pulls remote changes → cache (download / invalidate).
         │       Marks entries clean on success.
         │       Paused when connectivity monitor reports OFFLINE.
         │
         ├── Download Manager  (per-file goroutines)
         │       Manages in-progress remote → cache downloads.
         │       Tracks a per-file cached-ranges interval set: a sorted,
         │         merged list of [start, end) pairs covering bytes already
         │         written to the cache file.
         │       FUSE reads whose range is fully covered are served instantly.
         │       FUSE reads that fall in a gap block until a goroutine covers
         │         the requested range (or an error is signalled).
         │       Supports range-request seeks: spawns an additional goroutine
         │         for the new offset; the original goroutine is NOT cancelled,
         │         so existing sequential readers keep downloading uninterrupted.
         │
         ├── Connectivity Monitor  (background goroutine)
         │       Probes remote endpoint on a ticker (e.g. every 5s).
         │       State machine: ONLINE → OFFLINE → ONLINE
         │       Signals sync engine to pause/resume.
         │
         └── Remote Adapter  (interface)
                 Abstracts: List, Stat, Get, Put, Delete, Mkdir,
                            GetRange (range/partial download)
                 Implementations: S3Adapter, SFTPAdapter, ...
```

### Data Flow: Write

```
app write(fd, buf)
  → FUSE write handler
  → write buf to cache file on disk
  → mark path as dirty in Metadata DB
  → return success to app immediately

[background]
  Sync Engine picks up dirty entry
  → Remote Adapter.Put(path, cache_file)
  → on success: mark clean in Metadata DB
  → on failure: leave dirty, retry with backoff
```

### Data Flow: Read (cache hit)

```
app read(fd, buf)
  → FUSE read handler
  → check Metadata DB: is path cached?
  → yes: read from cache file on disk
  → return bytes to app
```

### Data Flow: Read (cache miss, online — streaming)

```
app open(path)
  → FUSE open handler
  → check Metadata DB: path not cached (state = evicted or absent)
  → Download Manager: spawn background downloader goroutine for path
  → mark path as state = 'downloading', cached_ranges = [] in DB
  → return fd to app immediately  ← does NOT wait for full download

app read(fd, offset, size)
  → FUSE read handler
  → Download Manager: rangeSet.Contains(offset, size)?
      yes → read from cache file on disk, return bytes immediately
      no  → block: register waiter for [offset, offset+size);
              wait for any goroutine to signal range-set update
              and re-check Contains on each wake-up
  → once Contains returns true: read from cache, return bytes

[background downloader goroutine (one per active range)]
  → Remote Adapter.Get/GetRange → streams chunk loop into cache file
  → after each written chunk: rangeSet.Add(chunkOffset, chunkLen),
      broadcast to all waiters (they re-check Contains themselves)
  → on completion: if rangeSet.IsComplete(totalSize) →
        mark state = 'clean', persist final cached_ranges,
        evict download record from Download Manager
  → on error: signal waiters with error; if no other goroutines are
        active for this file, mark state = 'evicted'
```

### Data Flow: Read (random seek while streaming)

```
app lseek(fd, seekOffset, SEEK_SET) + read(fd, buf, n)
  → FUSE read at seekOffset not covered by rangeSet

  Case A — backend supports range requests (S3, SFTP, HTTP):
      → Download Manager: spawn ADDITIONAL goroutine for
          adapter.GetRange(path, seekOffset, remainingBytes)
      → original sequential goroutine keeps running unaffected
      → both goroutines independently call rangeSet.Add() as chunks land
      → rangeSet.Contains(seekOffset, n) becomes true quickly;
          blocked FUSE read is woken and served from cache
      → when the two goroutines' ranges meet or overlap they are
          merged automatically; no duplicate bytes are written

  Case B — backend does not support range requests (e.g. no HTTP ranges):
      → only one sequential goroutine is allowed at a time
      → block and wait for the sequential downloader to reach seekOffset
      → serve as normal once Contains returns true
      (this is the slow path; the range set still advances in the background)
```

### Data Flow: Read (cache miss, offline)

```
app open(path)
  → FUSE open handler
  → check Metadata DB: path not cached
  → connectivity is OFFLINE
  → return ENOENT or stale-reads-only error
  → (file was never cached — nothing to serve)
```

---

## 4. Component Breakdown

### 4.1 FUSE Layer

Implements the standard FUSE operations by delegating entirely to the Cache Layer. Never touches the network.

Key operations to implement:

| Operation | Behavior                                                                                                                                                                                                  |
| --------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `getattr` | Read inode metadata from Metadata DB                                                                                                                                                                      |
| `readdir` | List directory entries from Metadata DB                                                                                                                                                                   |
| `open`    | If cached/downloading: return fd immediately. If miss + online: start Download Manager goroutine, mark `downloading`, return fd without waiting. If miss + offline: return `ENOENT`.                      |
| `read`    | Check `rangeSet.Contains(offset, size)`: if covered, read from cache file immediately; otherwise register a waiter and block until any Download Manager goroutine covers the range, then read from cache. |
| `write`   | Write to cache file, mark dirty                                                                                                                                                                           |
| `create`  | Create cache file, add to Metadata DB, mark dirty                                                                                                                                                         |
| `unlink`  | Remove cache file, mark path as `deleted_local` in DB                                                                                                                                                     |
| `mkdir`   | Create cache dir, mark dirty                                                                                                                                                                              |
| `rmdir`   | Remove cache dir, mark dirty                                                                                                                                                                              |
| `rename`  | Rename in cache + DB, mark dirty                                                                                                                                                                          |
| `release` | Flush kernel buffers; no-op for us since we write-through                                                                                                                                                 |

**Important:** `getattr` and `readdir` must work offline by serving from the Metadata DB. These are called by `ls`, `stat`, and any tool that browses the directory.

### 4.2 Cache Layer

The cache directory mirrors the remote path structure:

```
~/.cache/<appname>/
  <remote-id>/
    files/          ← actual file contents (mirroring remote structure)
      docs/
        report.pdf
        notes.txt
    meta.db         ← SQLite metadata database
    pending.db      ← SQLite sync queue / write journal
```

Responsibilities:

- Provide `Read(path, offset, size)`, `Write(path, data)`, `Delete(path)`, `List(path)` over the cache directory
- Manage cache size (LRU eviction of clean files when space is low)
- Pinning: mark certain paths to never be evicted (future feature)
- Coordinate with Download Manager so that reads to partially-downloaded files do not return stale bytes: reads always go through the Download Manager `rangeSet.Contains()` check first

### 4.3 Download Manager

Manages all in-progress remote → cache downloads. One download goroutine per file at most.

**Per-download state (held in memory):**

| Field        | Description                                                                 |
| ------------ | --------------------------------------------------------------------------- |
| `path`       | Remote path being downloaded                                                |
| `cacheFile`  | Open `*os.File` being written (shared by all goroutines for this path)      |
| `rangeSet`   | Interval set of `[start, end)` pairs; all bytes in these ranges are on disk |
| `totalSize`  | Expected file size from `Stat` (may be 0 if unknown)                        |
| `waiters`    | List of `(targetRange, chan error)` pairs; woken on every range-set update  |
| `goroutines` | Map of `startOffset → cancelFn`; one entry per active download goroutine    |

**Range-set signalling:**

After each goroutine writes a chunk it calls `rangeSet.Add(chunkOffset, chunkLen)` under a mutex, then broadcasts on a shared condition variable. All blocked waiters wake up, call `rangeSet.Contains(offset, size)`, and go back to sleep if not yet covered. An interval set guarantees that overlapping or adjacent chunks from concurrent goroutines are merged automatically — no byte is served from a gap.

**Random seek handling:**

When a FUSE `read` arrives at an offset not covered by `rangeSet` and the backend supports range requests, the Download Manager spawns an **additional** goroutine via `adapter.GetRange(path, seekOffset, ...)`. The original goroutine is left running. Both goroutines write into the same cache file (at their respective offsets) and register their progress independently into `rangeSet`. The seeking reader is unblocked as soon as its range is covered; there is no need to restart the sequential download.

**Deduplication:**

If two processes `open` the same path simultaneously, the Download Manager returns the same in-flight download to both. Both readers share one network fetch.

**Lifecycle:**

```
open(path) for the first time
  → Download Manager.Start(path)
  → goroutine: Get(path) → chunk loop → rangeSet.Add() → broadcast
  → on completion: if rangeSet.IsComplete(totalSize) → mark 'clean',
      persist cached_ranges to DB, remove entry from Download Manager
  → on error: broadcast error to waiters; if no other goroutines remain
      for this file, mark 'evicted' in DB

open(path) while already downloading
  → Download Manager.Attach(path)
  → returns current download record; caller registers a (range, chan) waiter

read(fd, offset, size) where rangeSet does not cover [offset, offset+size)
  and backend supports ranges
  → Download Manager.SpawnRangeGoroutine(path, offset)
  → new goroutine: GetRange(path, offset, ...) → chunk loop →
      rangeSet.Add() → broadcast
  → blocked waiter is woken when Contains(offset, size) = true
```

### 4.4 Metadata Database

SQLite database tracking every known path. See [Section 5](#5-data-model) for schema.

### 4.5 Sync Engine

A background goroutine that:

1. Waits for `ONLINE` signal from Connectivity Monitor
2. Queries Metadata DB for dirty/deleted entries ordered by mtime
3. For each entry, calls the appropriate Remote Adapter operation
4. On success, marks entry clean; on failure, logs error and applies exponential backoff
5. Periodically polls remote for changes (configurable interval, default 30s)
6. On detecting a remote change:
   - If local state is `clean`: invalidate cache, download new version
   - If local state is `dirty`: flag as `conflict`, do not overwrite local
7. Does **not** touch files in `downloading` state — the Download Manager owns those

Backoff schedule for failed syncs: 5s → 15s → 30s → 60s → 5min → 15min (cap).

### 4.6 Connectivity Monitor

Probes the remote on a configurable interval (default 5s). Probe strategy depends on backend:

- **S3**: `HeadObject` on a sentinel key, or `ListBuckets` with a short timeout
- **SFTP**: TCP connect to host:port + SSH handshake

State machine:

```
ONLINE ──(probe fails N times)──► OFFLINE
OFFLINE ──(probe succeeds)──► RECONNECTING ──(sync catches up)──► ONLINE
```

`RECONNECTING` is a transient state while the sync engine drains the dirty queue before declaring fully online. The CLI shows this state to the user.

When the monitor transitions to `OFFLINE`, in-flight Download Manager goroutines are cancelled immediately (their context is derived from a mount-level connectivity context). Files that were `downloading` are reset to `evicted` so the next `open` retries when connectivity returns.

### 4.7 Remote Adapters

A common interface implemented by each backend:

```go
type RemoteAdapter interface {
    List(path string) ([]FileInfo, error)
    Stat(path string) (FileInfo, error)
    Get(path string, dest io.Writer) error
    // GetRange downloads [offset, offset+length) of path into dest.
    // Backends that do not support range requests return ErrNoRangeSupport;
    // the Download Manager falls back to sequential Get in that case.
    GetRange(path string, offset, length int64, dest io.Writer) error
    Put(path string, src io.Reader, size int64, mtime time.Time) error
    Delete(path string) error
    Mkdir(path string) error
    Rename(src, dst string) error
    Probe() error   // connectivity check
    // SupportsRange reports whether this backend can serve range requests.
    SupportsRange() bool
}
```

**v1 backends (Phase 3):**

- `GDriveAdapter` — Google Drive

**v2 backends (Phase 7):**

- `SFTPAdapter` — any SFTP server
- `S3Adapter` — AWS S3, Cloudflare R2, MinIO, Backblaze B2

**future backends:**

- `WebDAVAdapter`
- `DropboxAdapter`

---

## 5. Data Model

### 5.1 `files` table (Metadata DB)

```sql
CREATE TABLE files (
    path              TEXT PRIMARY KEY,   -- full path relative to mount root
    is_dir            BOOLEAN NOT NULL DEFAULT 0,
    size              INTEGER,            -- bytes
    remote_mtime      INTEGER,            -- unix timestamp from remote
    local_mtime       INTEGER,            -- unix timestamp of cache file
    cache_path        TEXT,               -- absolute path to cached file on disk
    state             TEXT NOT NULL,      -- see states below
    cached_ranges     TEXT DEFAULT '[]',  -- JSON interval set, e.g. [[0,1048576],[2097152,4194304]]
                                          -- updated on goroutine completion/pause, not per-chunk
    sync_error        TEXT,               -- last sync error message, if any
    retry_after       INTEGER,            -- unix timestamp: don't retry before this
    checksum          TEXT                -- md5 or sha256 of cache file (set when complete)
);

CREATE INDEX idx_files_state ON files(state);
CREATE INDEX idx_files_dir   ON files(path) WHERE is_dir = 1;
```

**File states:**

| State            | Meaning                                                                                                             |
| ---------------- | ------------------------------------------------------------------------------------------------------------------- |
| `clean`          | Cache matches remote. No action needed.                                                                             |
| `dirty`          | Local write(s) pending sync to remote.                                                                              |
| `syncing`        | Sync engine is currently uploading this file.                                                                       |
| `downloading`    | Download Manager is actively streaming this file from remote. `cached_ranges` tracks which byte ranges are on disk. |
| `conflict`       | Both local and remote changed since last sync.                                                                      |
| `deleted_local`  | Deleted locally, needs to be deleted on remote.                                                                     |
| `deleted_remote` | Deleted on remote, local cache copy still exists.                                                                   |
| `evicted`        | File was in cache but evicted to free space. Re-download on next access.                                            |

### 5.2 `pending_ops` table (Write Journal)

An ordered log of operations that need to be synced. Complements the `files` table for rename/delete ordering.

```sql
CREATE TABLE pending_ops (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    op          TEXT NOT NULL,    -- 'put' | 'delete' | 'mkdir' | 'rmdir' | 'rename'
    path        TEXT NOT NULL,
    dest_path   TEXT,             -- for rename only
    queued_at   INTEGER NOT NULL, -- unix timestamp
    attempts    INTEGER DEFAULT 0,
    last_error  TEXT
);
```

---

## 6. Sync Engine Design

### 6.1 Upload (local → remote)

```
for each entry in files WHERE state = 'dirty' ORDER BY local_mtime ASC:
    if remote has newer version (remote_mtime > our last known remote_mtime):
        mark as 'conflict', skip
    else:
        call adapter.Put(path, cache_file)
        on success: update remote_mtime, set state = 'clean'
        on failure: increment retry count, set retry_after, leave dirty
```

### 6.2 Download / Remote Poll (remote → local)

```
for each path in remote listing:
    if path not in files DB:
        add to DB as evicted (will download on next FUSE access)
    else if remote_mtime > our known remote_mtime:
        if state = 'clean':
            download new version, update cache, update DB
        if state = 'dirty':
            mark as 'conflict' (do not overwrite local changes)

for each path in files DB not in remote listing:
    if state = 'clean':
        mark as 'deleted_remote' (remove from cache and DB)
    if state = 'dirty':
        remote was deleted but we have local changes
        → treat as conflict: re-upload or surface to user (configurable)
```

### 6.3 Open File During Disconnect

When a file is **already open** (fd exists in FUSE) and connectivity drops:

- **Fully downloaded file:** Data is in the cache. All reads/writes continue uninterrupted. No effect on the open handle.
- **Partially downloaded file (streaming in progress):**
  - The Download Manager goroutine's context is cancelled by the connectivity monitor.
  - Bytes already written to the cache file remain on disk and are still readable.
  - FUSE reads whose range is covered by `cached_ranges` continue to succeed.
  - FUSE reads blocked on an uncovered range receive `EIO` once all goroutines for that file have been cancelled.
  - The file state is reset to `evicted`; `cached_ranges` is persisted to the DB so a future reconnect can resume from the already-downloaded intervals (backends that support `GetRange`).
  - The app must re-open the file when connectivity is restored to trigger a fresh download.
- Writes always go straight to the cache and mark dirty, regardless of connectivity state.
- When `close()` is called: entry is marked dirty, sync engine queues it for upload when back online.

This works naturally because all FUSE ops go through cache. The connectivity monitor changing state has **no effect on fully-cached open file handles**.

### 6.4 Backoff & Retry

Failed sync attempts use exponential backoff to avoid hammering a recovering connection:

```
attempt 1: retry after 5s
attempt 2: retry after 15s
attempt 3: retry after 30s
attempt 4: retry after 60s
attempt 5: retry after 5min
attempt 6+: retry after 15min (cap)
```

A `sync --force` CLI command clears all `retry_after` timestamps and triggers immediate retry.

---

## 7. Conflict Resolution

A conflict occurs when both the local cache file and the remote file have been modified since the last known sync point.

### 7.1 Detection

Tracked via `remote_mtime` in the Metadata DB. If at sync time:

```
remote.mtime > files.remote_mtime  AND  files.state = 'dirty'
```

→ conflict.

### 7.2 Default Strategy (configurable per mount)

| Strategy      | Behavior                                                                                           |
| ------------- | -------------------------------------------------------------------------------------------------- |
| `local-wins`  | Overwrite remote with local version. Remote change is lost.                                        |
| `remote-wins` | Overwrite local with remote version. Local change is lost.                                         |
| `both`        | Keep local as-is, rename remote copy with `.conflict.<timestamp>` suffix. Both versions preserved. |
| `manual`      | Block sync for this file. Surface via CLI. User must resolve explicitly.                           |

Default: `both` — safest option, no data loss.

### 7.3 CLI Conflict Resolution

```bash
# List all conflicts
rvfs conflicts

# Output:
# ID   PATH                        LOCAL MTIME          REMOTE MTIME
# 1    /docs/report.pdf            2026-03-11 14:32      2026-03-11 15:10
# 2    /notes/meeting.txt          2026-03-11 09:00      2026-03-11 09:45

# Resolve: keep local, discard remote version
rvfs resolve 1 --keep local

# Resolve: keep remote, discard local changes
rvfs resolve 1 --keep remote

# Resolve: keep both (rename remote copy)
rvfs resolve 1 --keep both

# Resolve all with one strategy
rvfs resolve --all --keep local
```

---

## 8. CLI Interface

### 8.1 Commands

```bash
# Mount a remote
rvfs mount <remote>:<path> <mountpoint> [flags]

# Flags:
#   --cache-dir string         Cache directory (default: ~/.cache/yourapp)
#   --cache-size string        Max cache size, e.g. 50G (default: 20G)
#   --cache-max-age duration   Max duration that the cache should stay before being evicted (default: 0/does not expire)
#   --poll-interval duration   Remote poll interval (default: 30s)
#   --probe-interval duration  Connectivity probe interval (default: 5s)
#   --conflict string          Conflict strategy: local-wins|remote-wins|both|manual (default: both)

# Unmount
rvfs umount <mountpoint>

# Show status of all active mounts
rvfs status

# Show status of a specific mount
rvfs status <mountpoint>

# Show pending sync queue
rvfs queue [mountpoint]

# Force sync now (ignore backoff timers)
rvfs sync [mountpoint] [--force]

# List and resolve conflicts
rvfs conflicts [mountpoint]
rvfs resolve <conflict-id> --keep local|remote|both

# Pin paths (never evict from cache)
rvfs pin <path>
rvfs unpin <path>
rvfs pins [mountpoint]

# Show configured remotes
rvfs remotes

# Add a remote
rvfs remote add <type> <name>
rvfs remote add gdrive <name>
```

### 8.2 `status` Output Example

```
Mount:     ~/Documents
Remote:    gdocs:Documents
State:     ONLINE
Cache:     4.2 GB / 20.0 GB (21%)
Pending:   3 files to upload
Conflicts: 1 unresolved

Mount:     ~/Photos
Remote:    s3://mybucket/photos
State:     OFFLINE (last seen: 4 minutes ago)
Cache:     12.1 GB / 20.0 GB (61%)
Pending:   7 files to upload (queued, waiting for connection)
Conflicts: 0
```

### 8.3 `queue` Output Example

```
PRIORITY  PATH                        OP      SIZE    QUEUED
1         /docs/report.pdf            upload  2.1 MB  5 minutes ago
2         /docs/notes.txt             upload  4 KB    5 minutes ago
3         /old-file.txt               delete  —       8 minutes ago
4         /new-folder/               mkdir   —       12 minutes ago
```

---

## 9. Build Phases

### Phase 1 — FUSE Skeleton (no remote)

**Goal:** A working FUSE mount backed entirely by a local directory. Prove the FUSE layer works correctly.

- Set up Go project, module structure
- Implement FUSE operations using `hanwen/go-fuse`: `getattr`, `readdir`, `open`, `read`, `write`, `create`, `unlink`, `mkdir`, `rmdir`, `rename`
- Back all ops by a plain local directory (no cache DB yet)
- Manual test: `mount ./test-dir ~/mnt`, write files, read them back, `ls`, `cat`, `cp`, `vim`

**Exit criteria:** All basic POSIX file operations work through the mount. Standard tools (`cp`, `rsync`, `find`) behave correctly.

---

### Phase 2 — Cache Layer + Metadata DB

**Goal:** All FUSE ops go through SQLite-backed cache. Dirty tracking works.

- Create cache directory structure
- Implement Metadata DB schema (`files` table)
- Rewrite FUSE handlers to go through Cache Layer instead of direct disk
- Implement dirty tracking: every write → `state = dirty`
- Implement `pending_ops` journal

**Exit criteria:** After writing files through the mount, the Metadata DB correctly shows them as dirty. After a simulated "sync", they show as clean.

---

### Phase 3 — First Remote Adapter (Google Drive) + Streaming Downloads

**Goal:** Real remote reads and writes with streaming. `open()` returns immediately; data is served as it arrives.

- Implement `RemoteAdapter` interface (including `GetRange` / `SupportsRange`)
- Implement `GDriveAdapter`: List, Stat, Get, GetRange, Put, Delete, Mkdir, Rename, Probe
- OAuth2 authentication flow + token persistence (`rvfs auth gdrive`)
- Path→ID mapping layer (Drive uses file IDs, not paths)
- Implement Download Manager: per-file goroutine pool, in-memory range set (interval list with merge), waiter condition variable, concurrent range-seek via additional goroutines
- On cache miss: Download Manager starts streaming goroutine; FUSE reads served as data arrives
- Sync Engine v1: on mount, push all dirty entries to remote; pull remote listing into DB

**Exit criteria:** Can mount a Google Drive folder. Opening a large video file returns an fd immediately. `mpv` or `vlc` can play it while it's still downloading. Write files locally and have them appear on Drive.

---

### Phase 4 — Connectivity Monitor + Offline Mode

**Goal:** Seamless offline handling.

- Implement Connectivity Monitor with probe loop
- Wire ONLINE/OFFLINE signals to Sync Engine (pause/resume)
- Handle cache miss while offline: return `ENOENT` with a clear error
- Test: mount GDrive, write a file, disconnect network, write more files, reconnect — all pending writes sync

**Exit criteria:** Pulling the network cable (or blocking the remote host) doesn't crash the mount or return errors on already-cached files. Writes queue up and sync on reconnect.

---

### Phase 5 — Conflict Detection + Resolution

**Goal:** Conflicts are detected and surfaced, not silently lost.

- Implement conflict detection in Sync Engine
- Implement `both` strategy (default): rename remote conflicting copy
- Implement `conflicts` and `resolve` CLI commands
- Add `manual` strategy: block sync, require explicit resolution

**Exit criteria:** Simulate a conflict (edit file locally, edit same file on remote, sync) — conflict is detected, both versions preserved, CLI surfaces it.

---

### Phase 6 — CLI Polish + Cache Eviction

**Goal:** Production-ready CLI.

- Add `remote add` / `remote list` commands with credential storage
- Add `pin` / `unpin` commands
- Add `sync --force` command
- Status output with rich terminal formatting
- Configurable cache size with LRU eviction of clean files

**Exit criteria:** CLI is pleasant to use. Cache eviction keeps disk usage bounded.

---

### Phase 7 — Additional Adapters (SFTP + S3)

**Goal:** Bring in two more battle-tested backends.

- Implement `SFTPAdapter` (SSH key, password, agent auth)
- Implement `S3Adapter` (AWS SDK v2, compatible with R2, MinIO, B2)
- `rvfs remote add` supports `sftp://` and `s3://` URLs

**Exit criteria:** Can mount SFTP servers and S3 buckets. All three adapters (GDrive, SFTP, S3) can run as simultaneous mounts.

---

### Phase 8 — Hardening + Edge Cases

**Goal:** Handle the nasty real-world cases.

- Large file streaming: verify Download Manager handles files larger than available RAM; uploads stream from cache without buffering entire file
- Download resume: if a `downloading` file is found in the DB on mount startup (crash recovery), the `cached_ranges` JSON is already on disk. For backends that support range requests, issue `GetRange` for each gap in `cached_ranges` to resume without re-downloading completed intervals. Fall back to restart from offset 0 otherwise.
- Rename while dirty (update path in DB + pending_ops atomically)
- Delete while syncing or downloading (cancel in-flight goroutine, mark deleted)
- Concurrent writes to same file from multiple processes
- Cache corruption recovery (checksum mismatch → re-download from remote)
- Mount persistence across reboots (systemd unit / launchd plist)
- `--foreground` mode for debugging

---

## 10. Tech Stack

| Component       | Choice                                                       | Rationale                                                                  |
| --------------- | ------------------------------------------------------------ | -------------------------------------------------------------------------- |
| Language        | Go                                                           | Same ecosystem as FUSE libs; great concurrency; single binary distribution |
| FUSE bindings   | `hanwen/go-fuse/v2`                                          | Mature, production-used (Keybase, etc.), good documentation                |
| Metadata DB     | `modernc.org/sqlite`                                         | Pure Go SQLite, no CGo dependency, reliable                                |
| GDrive backend  | `google.golang.org/api/drive/v3` + `golang.org/x/oauth2`     | Official Go client; OAuth2 handled by google package                       |
| S3 backend      | `aws/aws-sdk-go-v2`                                          | Official SDK, supports S3-compatible APIs (R2, MinIO, B2) — Phase 7        |
| SFTP backend    | `pkg.go.dev/golang.org/x/crypto/ssh` + `github.com/pkg/sftp` | Well-maintained — Phase 7                                                  |
| CLI framework   | `spf13/cobra`                                                | Standard in Go ecosystem, same as kubectl/rclone                           |
| Config storage  | `~/.config/<appname>/config.toml`                            | Simple, human-readable                                                     |
| Terminal output | `github.com/charmbracelet/lipgloss`                          | Clean table/status formatting                                              |

---

## 11. Open Questions

- **Cache eviction policy:** LRU based on access time is simple. Should users be able to set per-directory cache policies (e.g. always cache `~/Documents/important/`)?

- **Remote polling vs. push notifications:** Google Drive, Dropbox, and OneDrive support webhook-style change notifications. Worth implementing in v2 for snappier remote change detection?

- **Partial file caching / streaming:** ~~Resolved~~ — the Download Manager design streams files from the remote and serves FUSE reads as data arrives. The cache file is written sequentially; a `downloaded_bytes` watermark gates reads. Random seeks trigger a `GetRange` restart on backends that support it (S3, SFTP, HTTP). There is no need to cache only specific byte ranges; the full file is always eventual on disk.

- **Download resume after crash:** If the process crashes while `downloading`, the DB retains `cached_ranges`. On next mount, the already-downloaded intervals are already on disk. For backends that support range requests, the Download Manager can issue `GetRange` for each gap in `cached_ranges` to resume rather than restart. For v1 the simpler approach is to reset `downloading → evicted` and restart from offset 0. Gap-filling resume is a v2 optimisation.

- **Concurrent streaming of the same file:** The Download Manager deduplicates: multiple processes opening the same path share the same in-memory `rangeSet` and goroutine pool. A seek from one reader spawns an additional `GetRange` goroutine for the missing range without disturbing the sequential goroutine that another reader is relying on. Both goroutines merge their progress into the shared `rangeSet`; all waiters wake up on every update and self-check. The main open issue is the goroutine cap: should there be a maximum number of concurrent range goroutines per file to avoid hammering the remote with too many simultaneous connections?

- **Encryption at rest:** Should the cache directory be encrypted? If the machine is compromised, cached files are exposed. Possible integration with system keychain for per-file encryption keys.

- **Multi-instance safety:** What happens if the same remote is mounted twice (two mounts pointing at the same remote path)? Need a lockfile or session registry to prevent split-brain.

- **FUSE on macOS:** `hanwen/go-fuse` targets Linux. macOS needs `bazil.org/fuse` or macFUSE. Should we target both platforms from the start, or Linux-first?

- **Conflict auto-resolution for directories:** The current design handles file conflicts. What happens when a directory is deleted on remote but files were added to it locally?
