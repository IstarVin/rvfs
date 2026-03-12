package download

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/testutil"
)

func newTestManager(t *testing.T, adapter *testutil.MockRemoteAdapter, opts ManagerOptions) (*Manager, *cache.CacheLayer) {
	t.Helper()
	cl, err := cache.NewCacheLayer(t.TempDir(), "test-remote")
	require.NoError(t, err)
	t.Cleanup(func() { cl.Close() })

	mgr := NewManager(adapter, cl, nil, opts)
	return mgr, cl
}

func TestManagerStartAndWaitForRange(t *testing.T) {
	content := bytes.Repeat([]byte("x"), 1024)
	adapter := &testutil.MockRemoteAdapter{
		GetFunc: func(path string, dest io.Writer) error {
			_, err := dest.Write(content)
			return err
		},
	}
	mgr, cl := newTestManager(t, adapter, ManagerOptions{})

	// Seed a DB entry so SetState doesn't fail.
	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path: "file.bin", State: cache.StateEvicted, Mode: 0100644,
		Size: int64(len(content)),
	}))

	dl, f, err := mgr.Start("file.bin", int64(len(content)))
	require.NoError(t, err)
	require.NotNil(t, dl)
	defer f.Close()

	// Wait for the full range.
	err = mgr.WaitForRange("file.bin", 0, int64(len(content)))
	require.NoError(t, err)
}

func TestManagerStartDedup(t *testing.T) {
	// Use a blocking Get so the download stays in-progress.
	started := make(chan struct{})
	adapter := &testutil.MockRemoteAdapter{
		GetFunc: func(path string, dest io.Writer) error {
			close(started)
			// Block until test ends.
			<-make(chan struct{})
			return nil
		},
	}
	mgr, cl := newTestManager(t, adapter, ManagerOptions{})

	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path: "dup.bin", State: cache.StateEvicted, Mode: 0100644,
		Size: 1024,
	}))

	dl1, f1, err := mgr.Start("dup.bin", 1024)
	require.NoError(t, err)
	defer f1.Close()

	<-started // Wait for download goroutine to begin.

	dl2, f2, err := mgr.Start("dup.bin", 1024)
	require.NoError(t, err)
	defer f2.Close()

	// Should return the same Download instance.
	assert.Equal(t, dl1, dl2, "second Start should return deduplicated download")

	mgr.Cancel("dup.bin")
}

func TestManagerIsDownloading(t *testing.T) {
	started := make(chan struct{})
	adapter := &testutil.MockRemoteAdapter{
		GetFunc: func(path string, dest io.Writer) error {
			close(started)
			<-make(chan struct{})
			return nil
		},
	}
	mgr, cl := newTestManager(t, adapter, ManagerOptions{})

	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path: "check.bin", State: cache.StateEvicted, Mode: 0100644,
		Size: 1024,
	}))

	assert.False(t, mgr.IsDownloading("check.bin"))

	_, f, err := mgr.Start("check.bin", 1024)
	require.NoError(t, err)
	defer f.Close()
	<-started

	assert.True(t, mgr.IsDownloading("check.bin"))

	mgr.Cancel("check.bin")

	assert.False(t, mgr.IsDownloading("check.bin"))
}

func TestManagerCancel(t *testing.T) {
	started := make(chan struct{})
	adapter := &testutil.MockRemoteAdapter{
		GetFunc: func(path string, dest io.Writer) error {
			close(started)
			<-make(chan struct{})
			return nil
		},
	}
	mgr, cl := newTestManager(t, adapter, ManagerOptions{})

	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path: "cancel.bin", State: cache.StateEvicted, Mode: 0100644,
		Size: 4096,
	}))

	_, f, err := mgr.Start("cancel.bin", 4096)
	require.NoError(t, err)
	defer f.Close()
	<-started

	mgr.Cancel("cancel.bin")
	assert.False(t, mgr.IsDownloading("cancel.bin"))
}

func TestManagerWaitForRangeUnknownPath(t *testing.T) {
	adapter := &testutil.MockRemoteAdapter{}
	mgr, _ := newTestManager(t, adapter, ManagerOptions{})

	// WaitForRange for a path that was never started should return nil.
	err := mgr.WaitForRange("nonexistent.bin", 0, 100)
	assert.NoError(t, err)
}

func TestManagerHintCachedNoop(t *testing.T) {
	content := bytes.Repeat([]byte("y"), 512)
	adapter := &testutil.MockRemoteAdapter{
		GetFunc: func(path string, dest io.Writer) error {
			_, err := dest.Write(content)
			return err
		},
	}
	mgr, cl := newTestManager(t, adapter, ManagerOptions{})

	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path: "hint.bin", State: cache.StateEvicted, Mode: 0100644,
		Size: int64(len(content)),
	}))

	_, f, err := mgr.Start("hint.bin", int64(len(content)))
	require.NoError(t, err)
	defer f.Close()

	// Wait for completion.
	require.NoError(t, mgr.WaitForRange("hint.bin", 0, int64(len(content))))

	// Hint for already-cached range should not panic.
	mgr.Hint("hint.bin", 0)
}

func TestManagerAlreadyCached(t *testing.T) {
	adapter := &testutil.MockRemoteAdapter{
		GetFunc: func(path string, dest io.Writer) error {
			return nil
		},
	}
	mgr, cl := newTestManager(t, adapter, ManagerOptions{})

	// Pre-populate a fully cached file.
	totalSize := int64(256)
	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path: "cached.bin", State: cache.StateEvicted, Mode: 0100644,
		Size: totalSize, CachedRanges: "[[0,256]]",
	}))
	// Create the file on disk too.
	cFile, err := cl.OpenOrCreate("cached.bin", totalSize)
	require.NoError(t, err)
	cFile.Close()

	_, f, err := mgr.Start("cached.bin", totalSize)
	require.NoError(t, err)
	defer f.Close()

	// Should complete immediately since fully cached.
	done := make(chan struct{})
	go func() {
		// Give a moment for the goroutine to mark done.
		time.Sleep(50 * time.Millisecond)
		close(done)
	}()
	<-done

	assert.False(t, mgr.IsDownloading("cached.bin"), "fully cached file should not remain in downloads")
}
