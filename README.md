# rvfs

[![CI](https://github.com/IstarVin/rvfs/actions/workflows/ci.yml/badge.svg)](https://github.com/IstarVin/rvfs/actions/workflows/ci.yml)
[![Release](https://github.com/IstarVin/rvfs/actions/workflows/release.yml/badge.svg)](https://github.com/IstarVin/rvfs/releases)

An offline-first FUSE filesystem that mounts remote storage (Google Drive, S3, SFTP) as a local directory. All reads and writes go through a local cache — the remote is an async sync target. Going offline is not a special mode; the sync engine simply pauses and resumes automatically when connectivity is restored.

## Features

- **Offline-first** — reads and writes always succeed locally, even with no network
- **Streaming downloads** — files are served from cache the moment bytes arrive; random seeks spawn parallel range-request goroutines
- **Background sync** — dirty files are pushed to the remote automatically; failures are retried with backoff
- **Connectivity-aware** — probes the remote on a configurable interval and pauses/resumes sync accordingly
- **Conflict detection** — parallel writes on the same file are detected and surfaced with resolution commands
- **Pinning** — mark files or directories to keep always cached, regardless of eviction policy
- **Configurable eviction** — LRU/LFU eviction with size limits and minimum free-space thresholds
- **Multiple remotes** — Google Drive supported today; S3 and SFTP planned

## Installation

### Pre-built binaries

Download the latest binary for your platform from the [Releases](https://github.com/IstarVin/rvfs/releases) page.

### From source

**Requirements:** Go 1.21+, FUSE kernel module (`fuse` on Linux, macOS FUSE on macOS)

```sh
git clone https://github.com/IstarVin/rvfs.git
cd rvfs
make build          # binary at bin/rvfs
make install        # installs to $GOPATH/bin
```

## Quick Start

### 1. Add a remote

```sh
rvfs remote add gdrive my-drive
# follow the OAuth prompts
```

### 2. Mount

```sh
rvfs mount my-drive:Documents ~/mnt/docs
```

The mountpoint is ready immediately. Files are streamed into the local cache on first access and synced back to the remote after writes.
On Linux, `df ~/mnt/docs` will show the RVFS mount. For remotes that expose quota information, RVFS reports the remote account's capacity and free space; for Google Drive subfolder mounts, those values are account-level rather than scoped to the mounted folder.

### 3. Check status

```sh
rvfs status        # connectivity state, sync queue depth, active downloads
rvfs queue         # pending upload/download operations
rvfs downloads     # in-progress streaming downloads
rvfs conflicts     # files with unresolved conflicts
```

### 4. Unmount

```sh
rvfs umount ~/mnt/docs
```

## CLI Reference

| Command                            | Description                                |
| ---------------------------------- | ------------------------------------------ |
| `rvfs mount <source> <mountpoint>` | Mount a remote or local directory via FUSE |
| `rvfs umount <mountpoint>`         | Unmount a FUSE mount                       |
| `rvfs remote add <type> <name>`    | Register a new remote backend              |
| `rvfs status`                      | Show sync engine and connectivity status   |
| `rvfs queue`                       | List pending sync operations               |
| `rvfs downloads`                   | List active streaming downloads            |
| `rvfs sync`                        | Trigger an immediate sync cycle            |
| `rvfs pin <path>`                  | Pin a file or directory (never evicted)    |
| `rvfs unpin <path>`                | Remove a pin                               |
| `rvfs pins`                        | List all pinned paths                      |
| `rvfs conflicts`                   | List files with unresolved conflicts       |

### `rvfs mount` flags

| Flag                  | Default         | Description                                         |
| --------------------- | --------------- | --------------------------------------------------- |
| `--foreground`        | `false`         | Run in the foreground (don't daemonise)             |
| `--debug`             | `false`         | Enable FUSE debug logging                           |
| `--cache-dir`         | `~/.cache/rvfs` | Override cache directory                            |
| `--cache-size`        | `10G`           | Maximum cache size                                  |
| `--cache-max-age`     | `168h`          | Maximum age of unused cached files                  |
| `--poll-interval`     | `30s`           | Remote change polling interval                      |
| `--probe-interval`    | `5s`            | Connectivity probe interval                         |
| `--read-ahead`        | `4M`            | Read-ahead buffer size                              |
| `--conflict-strategy` | `rename`        | Conflict resolution strategy (`rename`/`overwrite`) |
| `--install-service`   | `false`         | Install as a systemd/launchd service                |

## Configuration

Config is stored at `~/.config/rvfs/config.toml` (created on first run).

```toml
[mount]
cache_dir       = "~/.cache/rvfs"
cache_size      = "10G"
cache_max_age   = "168h"
poll_interval   = "30s"
probe_interval  = "5s"
read_ahead      = "4M"

[remotes.my-drive]
type        = "gdrive"
client_id   = "..."
root_path   = "Documents"
```

## Architecture

```
rvfs mount <remote>:<path> <mountpoint>
	│
	├── FUSE Layer         — serves all reads/writes from cache only
	├── Cache Layer        — real files on disk + SQLite metadata DB
	├── Sync Engine        — background goroutine pushing dirty files to remote
	├── Download Manager   — per-file streaming with range-set tracking
	├── Connectivity Monitor — probes remote; pauses/resumes sync engine
	└── Remote Adapter     — interface over gdrive / S3 / SFTP
```

See [docs/OVERVIEW.md](docs/OVERVIEW.md) for detailed design documentation.

## Development

```sh
make test           # run all tests
make test-fuse      # FUSE integration tests (requires FUSE kernel module)
make lint           # golangci-lint
make build          # build binary to bin/rvfs
```

## Platforms

| Platform             | Status               |
| -------------------- | -------------------- |
| Linux (amd64, arm64) | Supported            |
| macOS (amd64, arm64) | Supported            |
| Windows              | Not supported (FUSE) |

## License

MIT
