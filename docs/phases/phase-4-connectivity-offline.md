# Phase 4 — Connectivity Monitor + Offline Mode

> **Goal:** Seamless offline handling. Network loss doesn't crash the mount or return errors on already-cached files. Writes queue up and sync on reconnect.

---

## Key Steps

### 1. Connectivity Monitor

Create `internal/connectivity/monitor.go`:

**State machine:**

```
ONLINE ──(probe fails N times)──► OFFLINE
OFFLINE ──(probe succeeds)──► RECONNECTING ──(dirty queue drained)──► ONLINE
```

- `RECONNECTING` is a transient state while the sync engine catches up after reconnect — surfaced in CLI `status` output
- Probe method: calls `adapter.Probe()` on a configurable ticker (default 5s, flag `--probe-interval`)
- Configurable failure threshold before declaring OFFLINE (default: 3 consecutive failures)

**Exported API:**

- `State() ConnState` — current state
- `Subscribe() <-chan ConnState` — channel that emits on every state transition
- `Context() context.Context` — cancelled when transitioning to OFFLINE; renewed on ONLINE

### 2. Wire Sync Engine to connectivity signals

Modify `internal/sync/engine.go`:

- On `OFFLINE`: pause the upload loop and polling loop
- On `ONLINE` / `RECONNECTING`: resume — drain dirty queue, then poll remote
- Transition to `ONLINE` only after dirty queue is empty (or best-effort timeout)

### 3. Wire Download Manager to connectivity signals

Modify `internal/download/manager.go`:

- All download goroutines derive their `context.Context` from the monitor's `Context()` — automatically cancelled on OFFLINE
- On cancellation:
  - Persist current `cached_ranges` to DB
  - Set file state to `evicted`
  - Signal all waiters with `EIO`
  - Clean up goroutine map entry

### 4. Handle FUSE operations during offline

Modify `internal/fuse/node.go`:

- **Cache miss + OFFLINE:** `Open` returns `ENOENT`
- **Cache hit (any state):** all reads/writes continue normally — cache is the source of truth
- **Partially downloaded file on disconnect:**
  - Reads whose range is covered by `cached_ranges` → succeed
  - Reads in uncovered ranges → `EIO` (goroutine was cancelled)
- **Writes while offline:** go to cache + mark `dirty` as usual — no change in behavior

### 5. CLI visibility

Modify `internal/cli/mount.go`:

- Add `--probe-interval` flag (default `5s`)

The `status` command (Phase 6) will show ONLINE/OFFLINE/RECONNECTING, but the monitor must expose state for it now.

---

## Files to Create / Modify

| File                                    | Action | Purpose                                     |
| --------------------------------------- | ------ | ------------------------------------------- |
| `internal/connectivity/monitor.go`      | Create | Probe loop + state machine                  |
| `internal/connectivity/monitor_test.go` | Create | Unit tests with mock adapter                |
| `internal/sync/engine.go`               | Modify | Pause/resume on connectivity signals        |
| `internal/download/manager.go`          | Modify | Cancel downloads on OFFLINE, persist ranges |
| `internal/fuse/node.go`                 | Modify | ENOENT on cache miss + offline              |
| `internal/cli/mount.go`                 | Modify | Add `--probe-interval` flag                 |

---

## Exit Criteria

- [ ] Mount GDrive, write a file, disconnect network, write more files, reconnect — all pending writes sync
- [ ] `ls` and `cat` on fully cached files work while offline with no errors
- [ ] Opening an uncached file while offline returns `ENOENT`
- [ ] Partially-downloaded file: covered byte ranges readable, uncovered ranges return `EIO`
- [ ] `cached_ranges` are persisted to DB on disconnect (for future resume)
- [ ] Connectivity monitor correctly detects network loss within probe interval
- [ ] Reconnection triggers sync engine resume and dirty queue drain
- [ ] No goroutine leaks on disconnect — all download goroutines exit cleanly
