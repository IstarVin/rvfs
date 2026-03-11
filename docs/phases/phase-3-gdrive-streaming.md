# Phase 3 — Google Drive Adapter + Streaming Downloads

> **Goal:** Real remote reads/writes with Google Drive. `open()` returns immediately; data streams in via the Download Manager. Files written locally sync to Drive.

---

## Dependencies

| Package                          | Purpose                 |
| -------------------------------- | ----------------------- |
| `google.golang.org/api/drive/v3` | Google Drive API client |
| `golang.org/x/oauth2`            | OAuth2 authentication   |

---

## Key Steps

### 1. Remote adapter interface

Create `internal/remote/adapter.go` — define the common interface all backends implement:

```go
type RemoteAdapter interface {
    List(path string) ([]FileInfo, error)
    Stat(path string) (FileInfo, error)
    Get(path string, dest io.Writer) error
    GetRange(path string, offset, length int64, dest io.Writer) error
    Put(path string, src io.Reader, size int64, mtime time.Time) error
    Delete(path string) error
    Mkdir(path string) error
    Rename(src, dst string) error
    Probe() error
    SupportsRange() bool
}
```

Define `FileInfo` struct: `Path`, `Name`, `Size`, `IsDir`, `Mtime`, `Checksum`.

### 2. Google Drive adapter

Create `internal/remote/gdrive/gdrive.go`:

- OAuth2 flow: browser-based authorization, token persistence to `~/.config/rvfs/tokens/<remote-name>.json`
- **Path ↔ ID mapping:** Google Drive uses file IDs, not paths. Maintain an in-memory cache mapping `path → driveFileID`. Populate lazily via `List()` calls. Invalidate on rename/delete.
- Implement all `RemoteAdapter` methods using the Drive v3 API
- `GetRange`: use `alt=media` with an HTTP `Range` header
- `SupportsRange() → true` (Drive supports partial downloads)
- `Probe()`: lightweight `About.Get` call with short timeout

### 3. CLI auth & remote management

Create `internal/cli/remote.go`:

- `rvfs remote add gdrive <name>` — runs OAuth2 flow, stores token
- `rvfs auth gdrive <name>` — re-authenticate / refresh token

### 4. Download Manager

Create `internal/download/manager.go` — manages all in-progress remote→cache downloads:

**Per-download state (in memory):**

- `path` — remote path being downloaded
- `cacheFile` — open `*os.File` shared by all goroutines for this path
- `rangeSet` — interval set tracking which byte ranges are on disk
- `totalSize` — expected file size from `Stat`
- `waiters` — list of `(targetRange, chan error)` pairs
- `goroutines` — map of `startOffset → cancelFn`
- `mu` — mutex protecting all above
- `cond` — `sync.Cond` for signalling waiters

**Key methods:**

- `Start(path) → *Download` — spawns a sequential download goroutine
- `Attach(path) → *Download` — returns existing in-flight download (dedup)
- `SpawnRangeGoroutine(path, offset)` — spawns additional goroutine for random seek
- `WaitForRange(path, offset, size) error` — blocks until range is covered or error
- `Cancel(path)` — cancels all goroutines for a path

**Goroutine loop:** call `adapter.Get` or `adapter.GetRange` → read chunks → write to cache file at correct offset → `rangeSet.Add(chunkOffset, chunkLen)` → `cond.Broadcast()` → repeat until EOF or error.

### 5. Range set (interval set)

Create `internal/download/rangeset.go`:

- `RangeSet` — sorted, auto-merging list of `[start, end)` intervals
- `Add(offset, length)` — insert interval, merge overlapping/adjacent
- `Contains(offset, length) bool` — check if `[offset, offset+length)` is fully covered
- `IsComplete(totalSize) bool` — check if `[0, totalSize)` is covered
- `Gaps(totalSize) []Interval` — return uncovered intervals (for resume)
- `MarshalJSON` / `UnmarshalJSON` — serialize to `[[0,1024],[4096,8192]]` format for DB storage

### 6. Wire FUSE to Download Manager

Modify `internal/fuse/node.go`:

- `Open`: on cache miss (state `evicted` or absent), call `downloadManager.Start(path)` → mark state `downloading` → return fd immediately without waiting
- `Read`: call `downloadManager.WaitForRange(path, offset, size)` → once covered, read from cache file
- On cache hit (state `clean` or `dirty`): serve directly from cache, no Download Manager involvement

### 7. Sync Engine v1

Create `internal/sync/engine.go` — background goroutine:

- **Upload path:** query `files WHERE state = 'dirty' ORDER BY local_mtime ASC` → call `adapter.Put()` → on success: set `state = 'clean'`, update `remote_mtime`
- **Download/pull path:** call `adapter.List()` for the remote root → compare with DB:
  - New remote files → add to DB as `evicted`
  - Remote file with newer mtime + local `clean` → invalidate, re-download
  - Remote file with newer mtime + local `dirty` → mark `conflict` (Phase 5 handles resolution)
  - Remote file deleted + local `clean` → mark `deleted_remote`, remove cache
- Polling interval: `--poll-interval` flag (default 30s)

---

## Files to Create / Modify

| File                                 | Action | Purpose                                |
| ------------------------------------ | ------ | -------------------------------------- |
| `internal/remote/adapter.go`         | Create | `RemoteAdapter` interface + `FileInfo` |
| `internal/remote/gdrive/gdrive.go`   | Create | Google Drive adapter implementation    |
| `internal/download/manager.go`       | Create | Download Manager                       |
| `internal/download/rangeset.go`      | Create | Interval set data structure            |
| `internal/download/rangeset_test.go` | Create | Unit tests for RangeSet                |
| `internal/sync/engine.go`            | Create | Sync Engine v1                         |
| `internal/fuse/node.go`              | Modify | Wire Open/Read to Download Manager     |
| `internal/cli/remote.go`             | Create | `remote add`, `auth` commands          |
| `internal/cli/mount.go`              | Modify | Add `--poll-interval` flag             |

---

## Exit Criteria

- [ ] `rvfs remote add gdrive myaccount` completes OAuth2 flow and stores token
- [ ] `rvfs mount gdrive:Documents ~/mnt` mounts a Google Drive folder
- [ ] Opening a large file returns fd immediately (no blocking on full download)
- [ ] `mpv` / `vlc` can play a video file while it's still downloading
- [ ] Random seeks in a media player trigger `GetRange` goroutines without interrupting sequential download
- [ ] Two processes opening the same file share one network fetch (dedup)
- [ ] Files written locally through the mount appear on Google Drive after sync
- [ ] `adapter.List()` polling detects new remote files and adds them to DB
- [ ] RangeSet unit tests pass — merging, gaps, containment, serialization
