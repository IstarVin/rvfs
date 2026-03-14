package fuse

import (
	"context"
	"errors"
	"io"
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
