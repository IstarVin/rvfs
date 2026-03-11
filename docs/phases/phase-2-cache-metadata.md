# Phase 2 — Cache Layer + Metadata DB

> **Goal:** All FUSE ops go through a SQLite-backed cache layer. Dirty tracking works. This replaces the direct-backing-dir approach from Phase 1 with a structured cache that will later sit between FUSE and the remote.

---

## Dependencies

| Package              | Purpose                 |
| -------------------- | ----------------------- |
| `modernc.org/sqlite` | Pure Go SQLite (no CGo) |

---

## Key Steps

### 1. Cache directory layout

Create `internal/cache/layout.go` — manages the on-disk structure:

```
~/.cache/rvfs/<remote-id>/
  files/          ← actual file contents mirroring remote structure
  meta.db         ← SQLite metadata database
```

Provide helpers: `CacheDir(remoteID)`, `FilePath(remoteID, relativePath)`, `EnsureLayout(remoteID)`.

### 2. Metadata DB schema

Create `internal/cache/db.go` — open/create SQLite database, apply schema:

**`files` table:**

| Column          | Type    | Description                                                                                          |
| --------------- | ------- | ---------------------------------------------------------------------------------------------------- |
| `path`          | TEXT PK | Full path relative to mount root                                                                     |
| `is_dir`        | BOOLEAN | Directory flag                                                                                       |
| `size`          | INTEGER | File size in bytes                                                                                   |
| `remote_mtime`  | INTEGER | Unix timestamp from remote                                                                           |
| `local_mtime`   | INTEGER | Unix timestamp of cache file                                                                         |
| `cache_path`    | TEXT    | Absolute path to cached file on disk                                                                 |
| `state`         | TEXT    | `clean`, `dirty`, `syncing`, `downloading`, `conflict`, `deleted_local`, `deleted_remote`, `evicted` |
| `cached_ranges` | TEXT    | JSON interval set `[[start,end),...]`                                                                |
| `sync_error`    | TEXT    | Last sync error message                                                                              |
| `retry_after`   | INTEGER | Unix timestamp — don't retry before this                                                             |
| `checksum`      | TEXT    | md5/sha256 of cache file when complete                                                               |

Indexes: `idx_files_state` on `state`, `idx_files_dir` on `path WHERE is_dir = 1`.

**`pending_ops` table:**

| Column       | Type            | Description                                 |
| ------------ | --------------- | ------------------------------------------- |
| `id`         | INTEGER PK AUTO | Operation sequence ID                       |
| `op`         | TEXT            | `put`, `delete`, `mkdir`, `rmdir`, `rename` |
| `path`       | TEXT            | Target path                                 |
| `dest_path`  | TEXT            | Destination path (rename only)              |
| `queued_at`  | INTEGER         | Unix timestamp when queued                  |
| `attempts`   | INTEGER         | Retry count                                 |
| `last_error` | TEXT            | Last error message                          |

### 3. DB query helpers

Add methods to a `MetadataDB` struct:

- `GetFile(path) → FileEntry`
- `PutFile(entry FileEntry)` — insert or update
- `SetState(path, state)`
- `ListDir(dirPath) → []FileEntry`
- `ListByState(state) → []FileEntry`
- `AddPendingOp(op PendingOp)`
- `NextPendingOps(limit) → []PendingOp`
- `CompletePendingOp(id)`

### 4. Cache layer

Create `internal/cache/cache.go` with a `CacheLayer` struct that combines local file I/O with DB updates:

| Method                      | Behavior                                                                 |
| --------------------------- | ------------------------------------------------------------------------ |
| `Stat(path)`                | Read from `files` table                                                  |
| `ReadDir(path)`             | Query `files` table for children of `path`                               |
| `Read(path, offset, size)`  | Read from cache file on disk                                             |
| `Write(path, data, offset)` | Write to cache file + mark `dirty` in DB + append `put` to `pending_ops` |
| `Create(path, mode)`        | Create cache file + insert into DB as `dirty` + append `put`             |
| `Delete(path)`              | Remove cache file + mark `deleted_local` in DB + append `delete`         |
| `Mkdir(path, mode)`         | Create cache dir + insert into DB as `dirty` + append `mkdir`            |
| `Rmdir(path)`               | Remove cache dir + mark `deleted_local` + append `rmdir`                 |
| `Rename(old, new)`          | Rename cache file + update DB path + append `rename`                     |

All state-modifying methods run inside a SQLite transaction (file op + DB update atomic from the DB's perspective).

### 5. Refactor FUSE node

Modify `internal/fuse/node.go`: replace all direct `os.*` calls with `CacheLayer` method calls. The `FuseNode` now holds a reference to the shared `CacheLayer` instead of a raw backing dir path.

### 6. Wire up

- `Mount()` creates a `CacheLayer` for the given remote-id, passes it to the root `FuseNode`
- On first mount, `EnsureLayout()` creates the cache directory and initializes the DB
- CLI `mount` subcommand gains a `--cache-dir` flag (default `~/.cache/rvfs`)

---

## Files to Create / Modify

| File                       | Action | Purpose                                  |
| -------------------------- | ------ | ---------------------------------------- |
| `internal/cache/layout.go` | Create | Cache directory management               |
| `internal/cache/db.go`     | Create | SQLite schema, migrations, query helpers |
| `internal/cache/cache.go`  | Create | `CacheLayer` struct + methods            |
| `internal/fuse/node.go`    | Modify | Delegate all ops to `CacheLayer`         |
| `internal/fuse/mount.go`   | Modify | Create `CacheLayer` during mount setup   |
| `internal/cli/mount.go`    | Modify | Add `--cache-dir` flag                   |

---

## Exit Criteria

- [ ] Writing files through the mount populates both the cache directory and the `files` table
- [ ] `files` table shows `state = 'dirty'` for newly written/created files
- [ ] `pending_ops` records operations in chronological order
- [ ] Manually marking entries `clean` in the DB transitions state correctly
- [ ] `readdir` and `getattr` serve from the DB, not raw disk
- [ ] All Phase 1 integration tests still pass through the cache layer
- [ ] `make test` passes (add cache-layer unit tests)
