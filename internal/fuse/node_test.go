package fuse

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/connectivity"
	"github.com/IstarVin/rvfs/internal/download"
	"github.com/IstarVin/rvfs/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newEvictedNodeForTest(t *testing.T, adapter *testutil.MockRemoteAdapter, monitor *connectivity.Monitor, path string, size int64) (*FuseNode, *download.Manager, *cache.CacheLayer) {
	t.Helper()

	cl, err := cache.NewCacheLayer(t.TempDir(), "test-remote")
	require.NoError(t, err)
	t.Cleanup(func() { cl.Close() })

	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path:  path,
		State: cache.StateEvicted,
		Mode:  0100644,
		Size:  size,
	}))

	mgr := download.NewManager(adapter, cl, monitor, download.ManagerOptions{})
	node := &FuseNode{
		rel: path,
		root: &RootState{
			cache:       cl,
			downloadMgr: mgr,
			monitor:     monitor,
		},
	}

	return node, mgr, cl
}

func TestOpenEvictedDoesNotStartDownload(t *testing.T) {
	t.Parallel()

	adapter := &testutil.MockRemoteAdapter{}
	node, mgr, _ := newEvictedNodeForTest(t, adapter, nil, "lazy.txt", 64)

	fh, _, errno := node.Open(context.Background(), 0)
	require.Equal(t, syscall.Errno(0), errno)
	require.NotNil(t, fh)
	require.IsType(t, &downloadFileHandle{}, fh)

	assert.False(t, mgr.IsDownloading("lazy.txt"), "open should not start a download")
	assert.Len(t, adapter.CallsFor("Get"), 0, "open should not call remote Get")
	assert.Len(t, adapter.CallsFor("GetRange"), 0, "open should not call remote GetRange")

	require.Equal(t, syscall.Errno(0), fh.(*downloadFileHandle).Release(context.Background()))
}

func TestReadStartsDownloadForEvicted(t *testing.T) {
	t.Parallel()

	content := []byte("hello lazy download")
	adapter := &testutil.MockRemoteAdapter{
		GetFunc: func(_ context.Context, path string, dest io.Writer) error {
			_, err := dest.Write(content)
			return err
		},
	}
	node, mgr, _ := newEvictedNodeForTest(t, adapter, nil, "stream.txt", int64(len(content)))

	fh, _, errno := node.Open(context.Background(), 0)
	require.Equal(t, syscall.Errno(0), errno)
	dh := fh.(*downloadFileHandle)

	dest := make([]byte, len(content))
	_, readErrno := dh.Read(context.Background(), dest, 0)
	require.Equal(t, syscall.Errno(0), readErrno)
	assert.Equal(t, content, dest)
	assert.Len(t, adapter.CallsFor("Get"), 1, "first read should start remote download")

	require.Eventually(t, func() bool {
		return !mgr.IsDownloading("stream.txt")
	}, 2*time.Second, 10*time.Millisecond, "download should complete and be removed from manager")

	require.Equal(t, syscall.Errno(0), dh.Release(context.Background()))
}

func TestReadEvictedOfflineFailsAtReadNotOpen(t *testing.T) {
	t.Parallel()

	adapter := &testutil.MockRemoteAdapter{
		ProbeFunc: func(_ context.Context) error { return errors.New("offline") },
		GetFunc: func(_ context.Context, path string, dest io.Writer) error {
			_, err := dest.Write([]byte("unexpected"))
			return err
		},
	}
	monitor := connectivity.New(adapter, 5*time.Millisecond, 1)
	monitor.Start()
	t.Cleanup(func() { monitor.Stop() })

	require.Eventually(t, func() bool {
		return monitor.State() == connectivity.StateOffline
	}, time.Second, 5*time.Millisecond, "monitor should transition offline")

	node, mgr, _ := newEvictedNodeForTest(t, adapter, monitor, "offline.txt", 32)

	fh, _, errno := node.Open(context.Background(), 0)
	require.Equal(t, syscall.Errno(0), errno, "open should still succeed while offline")
	dh := fh.(*downloadFileHandle)

	_, readErrno := dh.Read(context.Background(), make([]byte, 8), 0)
	require.Equal(t, syscall.ENOENT, readErrno, "read should fail when offline")
	assert.False(t, mgr.IsDownloading("offline.txt"), "offline read should not start manager download")
	assert.Len(t, adapter.CallsFor("Get"), 0, "offline read should not call remote Get")

	require.Equal(t, syscall.Errno(0), dh.Release(context.Background()))
}

func TestConcurrentReadMultipleEvictedFiles(t *testing.T) {
	t.Parallel()

	const fileSize = 256
	contentFor := func(path string) []byte {
		return bytes.Repeat([]byte{path[0]}, fileSize)
	}

	adapter := &testutil.MockRemoteAdapter{
		GetFunc: func(_ context.Context, path string, dest io.Writer) error {
			_, err := dest.Write(contentFor(path))
			return err
		},
	}

	cl, err := cache.NewCacheLayer(t.TempDir(), "test-remote")
	require.NoError(t, err)
	t.Cleanup(func() { cl.Close() })

	mgr := download.NewManager(adapter, cl, nil, download.ManagerOptions{})
	rootState := &RootState{cache: cl, downloadMgr: mgr}

	paths := []string{"a.bin", "b.bin", "c.bin"}
	for _, p := range paths {
		require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
			Path:  p,
			State: cache.StateEvicted,
			Mode:  0100644,
			Size:  fileSize,
		}))
	}

	var wg sync.WaitGroup
	results := make([][]byte, len(paths))
	errnos := make([]syscall.Errno, len(paths))

	for i, p := range paths {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			node := &FuseNode{rel: p, root: rootState}
			fh, _, errno := node.Open(context.Background(), 0)
			if errno != 0 {
				errnos[i] = errno
				return
			}
			dh := fh.(*downloadFileHandle)
			dest := make([]byte, fileSize)
			_, readErrno := dh.Read(context.Background(), dest, 0)
			errnos[i] = readErrno
			results[i] = dest
			dh.Release(context.Background())
		}(i, p)
	}
	wg.Wait()

	for i, p := range paths {
		require.Equal(t, syscall.Errno(0), errnos[i], "file %s", p)
		assert.Equal(t, contentFor(p), results[i], "content mismatch for %s", p)
	}
}

func TestFileHandleWriteUpdatesDBSize(t *testing.T) {
	t.Parallel()

	cl, err := cache.NewCacheLayer(t.TempDir(), "test-remote")
	require.NoError(t, err)
	t.Cleanup(func() { cl.Close() })

	f, _, err := cl.Create("write-size.txt", 0644)
	require.NoError(t, err)

	fh := &fileHandle{
		f:   f,
		rel: "write-size.txt",
		root: &RootState{
			cache: cl,
		},
	}

	written, errno := fh.Write(context.Background(), []byte("hello world"), 0)
	require.Equal(t, syscall.Errno(0), errno)
	require.Equal(t, uint32(len("hello world")), written)
	require.Equal(t, syscall.Errno(0), fh.Release(context.Background()))

	entry, err := cl.DB().GetFile("write-size.txt")
	require.NoError(t, err)
	require.NotNil(t, entry)
	assert.Equal(t, int64(len("hello world")), entry.Size)
	assert.Equal(t, cache.StateDirty, entry.State)

	info, err := os.Stat(cl.DiskPath("write-size.txt"))
	require.NoError(t, err)
	assert.Equal(t, info.Size(), entry.Size)
	ops, err := cl.DB().NextPendingOps(10)
	require.NoError(t, err)
	require.Len(t, ops, 1)
	assert.Equal(t, "put", ops[0].Op)
	assert.Equal(t, "write-size.txt", ops[0].Path)
}
