# Phase 7 — Additional Adapters (SFTP + S3)

> **Goal:** Two more battle-tested backends: SFTP and S3-compatible storage. All three adapters can run simultaneously.

---

## Dependencies

| Package                   | Purpose                                    |
| ------------------------- | ------------------------------------------ |
| `aws/aws-sdk-go-v2`       | AWS S3 SDK (compatible with R2, MinIO, B2) |
| `golang.org/x/crypto/ssh` | SSH transport for SFTP                     |
| `github.com/pkg/sftp`     | SFTP client                                |

---

## Key Steps

### 1. SFTP adapter

Create `internal/remote/sftp/sftp.go`:

**Authentication methods:**

- SSH private key (path configurable)
- Password
- SSH agent forwarding

**RemoteAdapter implementation:**

- `List` / `Stat` — standard SFTP `ReadDir` / `Stat`
- `Get` — `sftp.Open` + `io.Copy` to dest
- `GetRange` — `sftp.Open` + `file.Seek(offset)` + `io.CopyN` → `SupportsRange() = true`
- `Put` — `sftp.Create` + `io.Copy` from src, set mtime via `sftp.Chtimes`
- `Delete` / `Mkdir` / `Rename` — standard SFTP ops
- `Probe` — TCP connect to `host:port` + SSH handshake with short timeout

### 2. S3 adapter

Create `internal/remote/s3/s3.go`:

**Configuration:**

- Bucket name, prefix (key path)
- Region
- Endpoint URL (for non-AWS S3-compatible: Cloudflare R2, MinIO, Backblaze B2)
- Access key ID + secret (or IAM role / instance profile)

**RemoteAdapter implementation:**

- `List` — `ListObjectsV2` with prefix + delimiter for directory-like listing
- `Stat` — `HeadObject`
- `Get` — `GetObject` → stream body to dest
- `GetRange` — `GetObject` with `Range` header → `SupportsRange() = true`
- `Put` — `PutObject` from src reader, set `Content-Length`, store mtime in metadata
- `Delete` — `DeleteObject`
- `Mkdir` — no-op (S3 has no real directories) or create a zero-byte `key/` marker
- `Rename` — `CopyObject` + `DeleteObject` (S3 has no native rename)
- `Probe` — `HeadBucket` or `ListObjectsV2` with `MaxKeys=1` and short timeout

### 3. Extend CLI

Modify `internal/cli/remote.go`:

- `rvfs remote add sftp <name>` — prompt for host, port, username, auth method
- `rvfs remote add s3 <name>` — prompt for bucket, region, endpoint, credentials

### 4. Extend config

Modify `internal/config/config.go`:

- SFTP fields: host, port, username, key_path, password (stored securely or referenced)
- S3 fields: bucket, region, endpoint, access_key_id, secret_access_key

### 5. Adapter factory

Create or update `internal/remote/factory.go`:

- `NewAdapter(remoteType, config) → RemoteAdapter` — returns the correct adapter based on type string (`gdrive`, `sftp`, `s3`)

---

## Files to Create / Modify

| File                           | Action | Purpose                                 |
| ------------------------------ | ------ | --------------------------------------- |
| `internal/remote/sftp/sftp.go` | Create | SFTP adapter                            |
| `internal/remote/s3/s3.go`     | Create | S3 adapter                              |
| `internal/remote/factory.go`   | Create | Adapter factory                         |
| `internal/cli/remote.go`       | Modify | Support `sftp` and `s3` in `remote add` |
| `internal/config/config.go`    | Modify | Adapter-specific config fields          |

---

## Exit Criteria

- [ ] `rvfs mount sftp://user@host:/path ~/mnt` mounts an SFTP server
- [ ] `rvfs mount s3://bucket/prefix ~/mnt` mounts an S3 bucket
- [ ] Range requests work on both backends — streaming video playback via `mpv`/`vlc`
- [ ] All three adapters (GDrive, SFTP, S3) can run as simultaneous mounts without interference
- [ ] Connectivity probe works per-backend (SSH handshake for SFTP, HeadBucket for S3)
- [ ] S3-compatible endpoints (R2, MinIO, B2) work with custom endpoint URL
- [ ] Dirty files sync to SFTP/S3 correctly
- [ ] Remote polling detects changes on SFTP/S3
