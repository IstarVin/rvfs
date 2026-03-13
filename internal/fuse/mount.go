package fuse

import (
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/connectivity"
	"github.com/IstarVin/rvfs/internal/download"
	"github.com/IstarVin/rvfs/internal/remote"
	syncpkg "github.com/IstarVin/rvfs/internal/sync"
	"github.com/hanwen/go-fuse/v2/fs"
	gofuse "github.com/hanwen/go-fuse/v2/fuse"
)

// MountOptions configures FUSE mount behaviour.
type MountOptions struct {
	// Debug enables verbose FUSE debug logging.
	Debug bool
	// Name is the filesystem name shown in df output.
	Name string
	// Adapter is the remote storage backend. Nil for backing-dir mode.
	Adapter remote.RemoteAdapter
	// Monitor is the connectivity monitor. Nil for backing-dir mode.
	// When non-nil, downloads are cancelled on OFFLINE and Open returns
	// ENOENT for uncached files while offline.
	Monitor *connectivity.Monitor
	// ReadAhead is the maximum number of bytes the sequential download
	// goroutine may get ahead of the reader's current position.
	// 0 means unlimited (download as fast as possible).
	ReadAhead int64
	// IdleTimeout stops the sequential download goroutine when it has been
	// paused at the read-ahead limit for this duration with no reads.
	// The goroutine restarts automatically on the next read.
	// 0 means wait indefinitely. Only meaningful when ReadAhead > 0.
	IdleTimeout time.Duration
	// SyncEngine is the background sync engine. When non-nil, uploads in
	// flight are cancelled when the owning file is unlinked.
	SyncEngine *syncpkg.Engine
	// VerifyChecksums, when true, re-hashes clean cache files on Open and
	// evicts them if the checksum does not match the stored value.
	VerifyChecksums bool
	// DownloadManager allows callers to inject a shared download manager.
	// When nil and Adapter is non-nil, Mount creates a manager automatically.
	DownloadManager *download.Manager
}

// Mount creates a CacheLayer for the given remote-id, mounts it at mountpoint,
// and returns the running FUSE server.
// The caller should call server.Wait() to block, and signal handling is
// set up automatically to unmount on SIGINT/SIGTERM.
func Mount(cacheBase, remoteID, mountpoint string, opts MountOptions) (*cache.CacheLayer, *gofuse.Server, error) {
	if err := os.MkdirAll(mountpoint, 0755); err != nil {
		return nil, nil, err
	}

	cl, err := cache.NewCacheLayer(cacheBase, remoteID)
	if err != nil {
		return nil, nil, err
	}

	rootState := &RootState{cache: cl}

	if opts.DownloadManager != nil {
		rootState.downloadMgr = opts.DownloadManager
	} else if opts.Adapter != nil {
		// If a remote adapter is provided, create a Download Manager.
		rootState.downloadMgr = download.NewManager(opts.Adapter, cl, opts.Monitor, download.ManagerOptions{
			ReadAhead:   opts.ReadAhead,
			IdleTimeout: opts.IdleTimeout,
		})
	}
	if opts.Monitor != nil {
		rootState.monitor = opts.Monitor
	}
	if opts.SyncEngine != nil {
		rootState.syncEngine = opts.SyncEngine
	}
	rootState.verifyChecksums = opts.VerifyChecksums

	root := &FuseNode{
		rel:  "",
		root: rootState,
	}

	fsName := opts.Name
	if fsName == "" {
		fsName = "rvfs"
	}

	// Root StableAttr must be set via fs.Options.RootStableAttr, not via NewInode.
	fsOpts := &fs.Options{
		RootStableAttr: &fs.StableAttr{
			Mode: syscall.S_IFDIR | 0755,
			Ino:  inodeFor(""),
		},
		MountOptions: gofuse.MountOptions{
			Debug:       opts.Debug,
			Name:        fsName,
			FsName:      remoteID,
			AllowOther:  true,
			MaxWrite:    1 << 20, // 1 MiB write buffer
			EnableLocks: true,
		},
	}

	server, err := fs.Mount(mountpoint, root, fsOpts)
	if err != nil {
		cl.Close()
		return nil, nil, err
	}

	// Unmount cleanly on SIGINT or SIGTERM.
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		_ = server.Unmount()
	}()

	return cl, server, nil
}
