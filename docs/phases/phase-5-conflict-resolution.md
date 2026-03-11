# Phase 5 — Conflict Detection + Resolution

> **Goal:** Conflicts are detected and surfaced to the user. No data is silently lost.

---

## Key Steps

### 1. Conflict detection in Sync Engine

Modify `internal/sync/engine.go` — during the upload path:

```
for each dirty file:
    remote_stat = adapter.Stat(path)
    if remote_stat.Mtime > files.remote_mtime:
        // Remote changed since our last sync point AND we have local changes
        → mark state = 'conflict'
        → skip upload
    else:
        → proceed with upload
```

During the pull/poll path:

```
for each remote file with newer mtime:
    if local state == 'clean':
        → invalidate cache, download new version
    if local state == 'dirty':
        → mark state = 'conflict'
        → do NOT overwrite local changes
```

### 2. Conflict strategies

Create `internal/sync/conflict.go`:

| Strategy         | Behavior                                                                                                                                     |
| ---------------- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| `both` (default) | Keep local file as-is. Download remote version alongside it with a `.conflict.<timestamp>` suffix. Both versions preserved — zero data loss. |
| `local-wins`     | Upload local version to remote, overwriting the remote change. Remote change is lost.                                                        |
| `remote-wins`    | Download remote version, overwriting local cache. Local change is lost.                                                                      |
| `manual`         | Block sync for this file entirely. User must resolve via CLI before sync resumes for this path.                                              |

Strategy is configurable per-mount via `--conflict` flag (default: `both`).

### 3. `conflicts` CLI command

Create `internal/cli/conflicts.go`:

- `rvfs conflicts [mountpoint]` — query `files WHERE state = 'conflict'`, display as table:
  ```
  ID   PATH                        LOCAL MTIME          REMOTE MTIME
  1    /docs/report.pdf            2026-03-11 14:32     2026-03-11 15:10
  2    /notes/meeting.txt          2026-03-11 09:00     2026-03-11 09:45
  ```

### 4. `resolve` CLI command

Add to `internal/cli/conflicts.go`:

- `rvfs resolve <id> --keep local` — upload local version, mark `dirty` (triggers sync)
- `rvfs resolve <id> --keep remote` — download remote version, overwrite cache, mark `clean`
- `rvfs resolve <id> --keep both` — keep both files (local + `.conflict` copy), mark `clean`
- `rvfs resolve --all --keep <strategy>` — batch resolve all conflicts

### 5. Mount flag

Modify `internal/cli/mount.go`:

- Add `--conflict` flag with choices: `local-wins`, `remote-wins`, `both`, `manual` (default: `both`)
- Pass strategy to Sync Engine configuration

---

## Files to Create / Modify

| File                        | Action | Purpose                               |
| --------------------------- | ------ | ------------------------------------- |
| `internal/sync/conflict.go` | Create | Conflict strategy implementations     |
| `internal/sync/engine.go`   | Modify | Detect conflicts during upload + pull |
| `internal/cli/conflicts.go` | Create | `conflicts` + `resolve` CLI commands  |
| `internal/cli/mount.go`     | Modify | Add `--conflict` flag                 |

---

## Exit Criteria

- [ ] Edit file locally + edit same file on remote + trigger sync → conflict detected
- [ ] Default `both` strategy: both versions preserved, `.conflict.<timestamp>` file created
- [ ] `rvfs conflicts` lists all conflicts with paths and timestamps
- [ ] `rvfs resolve <id> --keep local` uploads local version, clears conflict
- [ ] `rvfs resolve <id> --keep remote` downloads remote version, clears conflict
- [ ] `rvfs resolve --all --keep both` batch-resolves all conflicts
- [ ] `manual` strategy blocks sync for conflicted files until explicitly resolved
- [ ] No data loss under any conflict scenario with default `both` strategy
