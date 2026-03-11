package fuse

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/hanwen/go-fuse/v2/fs"
	gofuse "github.com/hanwen/go-fuse/v2/fuse"
)

// MountOptions configures FUSE mount behaviour.
type MountOptions struct {
	// Debug enables verbose FUSE debug logging.
	Debug bool
	// Name is the filesystem name shown in df output.
	Name string
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

	root := &FuseNode{
		rel:  "",
		root: &RootState{cache: cl},
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
