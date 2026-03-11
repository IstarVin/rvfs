# Phase 1 — FUSE Skeleton (No Remote)

> **Goal:** A working FUSE mount backed entirely by a local directory. Prove the FUSE layer works correctly before adding caching or remotes.

---

## Dependencies

| Package             | Purpose                    |
| ------------------- | -------------------------- |
| `hanwen/go-fuse/v2` | FUSE bindings (NodeFS API) |

---

## Key Steps

### 1. Define the core FUSE node

Create `internal/fuse/node.go` with a `FuseNode` struct embedding `fs.Inode`. Each node holds a `relativePath` (relative to the backing directory root) and a reference to the shared root state (backing dir path, etc.).

### 2. Implement FUSE operations

Implement the following `fs` interfaces on `FuseNode`:

| Interface       | Operation | Behavior                                                               |
| --------------- | --------- | ---------------------------------------------------------------------- |
| `NodeLookuper`  | `Lookup`  | `os.Lstat` on backing path → populate `EntryOut` from `syscall.Stat_t` |
| `NodeGetattrer` | `Getattr` | `syscall.Lstat` → fill `fuse.Attr` from `Stat_t`                       |
| `NodeSetattrer` | `Setattr` | `os.Chmod`, `os.Truncate`, `os.Chtimes` on backing path                |
| `NodeReaddirer` | `Readdir` | `os.ReadDir` on backing path → emit `fuse.DirEntry` list               |
| `NodeOpener`    | `Open`    | `os.OpenFile` on backing path → return `FileHandle`                    |
| `NodeCreater`   | `Create`  | `os.Create` on backing path → add child inode, return `FileHandle`     |
| `NodeReader`    | `Read`    | `file.ReadAt(buf, offset)` on the open file handle                     |
| `NodeWriter`    | `Write`   | `file.WriteAt(data, offset)` on the open file handle                   |
| `NodeUnlinker`  | `Unlink`  | `os.Remove` on backing path → remove child inode                       |
| `NodeMkdirer`   | `Mkdir`   | `os.Mkdir` on backing path → add child inode                           |
| `NodeRmdirer`   | `Rmdir`   | `os.Remove` on backing path → remove child inode                       |
| `NodeRenamer`   | `Rename`  | `os.Rename` on backing path → move child inode                         |

### 3. Mount/unmount logic

Create `internal/fuse/mount.go` with:

- `Mount(backingDir, mountpoint string, opts MountOptions) (*fuse.Server, error)` — sets up `fs.Options`, creates the root `FuseNode`, calls `fs.Mount`
- Signal handling to call `server.Unmount()` on SIGINT/SIGTERM

### 4. CLI `mount` subcommand

Create `internal/cli/mount.go`:

- `rvfs mount <backing-dir> <mountpoint>` — for Phase 1, the "remote" arg is just a local directory
- Register in `root.go`'s `init()`

### 5. Integration tests

Create `internal/fuse/fuse_test.go`:

- Mount a `t.TempDir()` as backing dir to another tmpdir mountpoint
- Test: create file, read back, stat, write, truncate, rename, unlink
- Test: mkdir, readdir, rmdir
- Test: `cp` of files (validates inode stability)
- Teardown: unmount in `t.Cleanup()`

---

## Known go-fuse v2 Pitfalls (from prior implementation)

These are hard-won lessons that **must** be applied during implementation:

1. **POSIX mode bits, not Go mode bits.** `os.FileMode` uses Go-specific constants (`ModeDir = 1<<31`). For `fuse.Attr.Mode`, always use the raw `syscall.Stat_t.Mode` field which is already POSIX-encoded.

2. **Inode numbering via FNV-64a hash of `relativePath`.** `StableAttr.Ino` controls the `st_ino` reported to userspace.
   - If `Ino == 0`: go-fuse auto-assigns sequential IDs starting at `1<<63`. These change after forget+recreate, causing `cp` "replaced while being copied" errors.
   - If using the backing file's `sys.Ino`: renames cause stale `relativePath` (same backing inode → same FUSE inode → wrong path lookup).
   - **Fix:** compute `hash/fnv.New64a()` of the `relativePath` string for each node's `StableAttr.Ino`.

3. **Root node `StableAttr`.** Set via `fs.Options.RootStableAttr`, **not** via `NewInode` on the root.

4. **Package locations.** `AttrOut` and `EntryOut` live in `github.com/hanwen/go-fuse/v2/fuse`, not the `fs` package.

5. **`SetAttrIn` field names.** The fields are `Atimensec` / `Mtimensec` (not `AtimeNsec` / `MtimeNsec`).

6. **Mkdir mode check.** The go-fuse bridge asserts `out.Attr.Mode &^ 07777 == syscall.S_IFDIR`. The mode must come from `Stat_t.Mode` (which includes `S_IFDIR`), not from `os.FileMode`.

---

## Files to Create / Modify

| File                         | Action | Purpose                                         |
| ---------------------------- | ------ | ----------------------------------------------- |
| `internal/fuse/node.go`      | Create | `FuseNode` struct + all FUSE op implementations |
| `internal/fuse/mount.go`     | Create | Mount/unmount logic, signal handling            |
| `internal/fuse/fuse_test.go` | Create | Integration tests                               |
| `internal/cli/mount.go`      | Create | `mount` cobra subcommand                        |
| `internal/cli/root.go`       | Modify | Register `mount` subcommand in `init()`         |

---

## Exit Criteria

- [ ] `rvfs mount ./test-dir ~/mnt` mounts successfully
- [ ] `ls`, `cat`, `cp`, `rsync`, `find`, `vim` all work correctly through the mount
- [ ] `cp` of large files completes without "replaced while being copied" error
- [ ] File permissions are preserved (POSIX mode bits)
- [ ] `make test-fuse` passes all integration tests
- [ ] Clean unmount on SIGINT/SIGTERM
