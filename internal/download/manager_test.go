package download

import (
	"bytes"
	"context"
	"encoding/json"
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
	t.Parallel()
	content := bytes.Repeat([]byte("x"), 1024)
	adapter := &testutil.MockRemoteAdapter{
		GetFunc: func(_ context.Context, path string, dest io.Writer) error {
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
	t.Parallel()
	// Use a blocking Get so the download stays in-progress.
	started := make(chan struct{})
	adapter := &testutil.MockRemoteAdapter{
		GetFunc: func(_ context.Context, path string, dest io.Writer) error {
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
	t.Parallel()
	started := make(chan struct{})
	adapter := &testutil.MockRemoteAdapter{
		GetFunc: func(_ context.Context, path string, dest io.Writer) error {
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
	t.Parallel()
	started := make(chan struct{})
	adapter := &testutil.MockRemoteAdapter{
		GetFunc: func(_ context.Context, path string, dest io.Writer) error {
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
	t.Parallel()
	adapter := &testutil.MockRemoteAdapter{}
	mgr, _ := newTestManager(t, adapter, ManagerOptions{})

	// WaitForRange for a path that was never started should return nil.
	err := mgr.WaitForRange("nonexistent.bin", 0, 100)
	assert.NoError(t, err)
}

func TestManagerHintCachedNoop(t *testing.T) {
	t.Parallel()
	content := bytes.Repeat([]byte("y"), 512)
	adapter := &testutil.MockRemoteAdapter{
		GetFunc: func(_ context.Context, path string, dest io.Writer) error {
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
	t.Parallel()
	adapter := &testutil.MockRemoteAdapter{
		GetFunc: func(_ context.Context, path string, dest io.Writer) error {
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

// ---------- Extended tests ----------

func TestManagerConcurrentStarts(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	adapter := &testutil.MockRemoteAdapter{
		GetFunc: func(_ context.Context, path string, dest io.Writer) error {
			close(started)
			<-make(chan struct{})
			return nil
		},
	}
	mgr, cl := newTestManager(t, adapter, ManagerOptions{})

	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path: "race.bin", State: cache.StateEvicted, Mode: 0100644,
		Size: 2048,
	}))

	// Launch multiple goroutines starting the same path concurrently.
	const n = 10
	type result struct {
		dl  *Download
		err error
	}
	results := make(chan result, n)

	for range n {
		go func() {
			dl, f, err := mgr.Start("race.bin", 2048)
			if f != nil {
				f.Close()
			}
			results <- result{dl, err}
		}()
	}

	// Wait for the download goroutine to actually start.
	<-started

	var downloads []*Download
	for range n {
		r := <-results
		require.NoError(t, r.err)
		downloads = append(downloads, r.dl)
	}

	// All should return the same Download instance.
	for i := 1; i < len(downloads); i++ {
		assert.Equal(t, downloads[0], downloads[i],
			"goroutine %d got different Download instance", i)
	}

	mgr.Cancel("race.bin")
}

func TestManagerMultipleFiles(t *testing.T) {
	t.Parallel()
	adapter := &testutil.MockRemoteAdapter{
		GetFunc: func(_ context.Context, path string, dest io.Writer) error {
			_, err := dest.Write(bytes.Repeat([]byte("z"), 256))
			return err
		},
	}
	mgr, cl := newTestManager(t, adapter, ManagerOptions{})

	for _, name := range []string{"file1.bin", "file2.bin", "file3.bin"} {
		require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
			Path: name, State: cache.StateEvicted, Mode: 0100644,
			Size: 256,
		}))
	}

	// Start all three downloads.
	for _, name := range []string{"file1.bin", "file2.bin", "file3.bin"} {
		_, f, err := mgr.Start(name, 256)
		require.NoError(t, err)
		f.Close()
	}

	// Wait for all to complete.
	for _, name := range []string{"file1.bin", "file2.bin", "file3.bin"} {
		require.NoError(t, mgr.WaitForRange(name, 0, 256))
	}

	// Give the download goroutines time to finish cleanup.
	require.Eventually(t, func() bool {
		for _, name := range []string{"file1.bin", "file2.bin", "file3.bin"} {
			if mgr.IsDownloading(name) {
				return false
			}
		}
		return true
	}, 2*time.Second, 10*time.Millisecond, "downloads should finish")
}

func TestManagerDownloadError(t *testing.T) {
	t.Parallel()
	adapter := &testutil.MockRemoteAdapter{
		GetFunc: func(_ context.Context, path string, dest io.Writer) error {
			return io.ErrUnexpectedEOF
		},
	}
	mgr, cl := newTestManager(t, adapter, ManagerOptions{})

	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path: "err.bin", State: cache.StateEvicted, Mode: 0100644,
		Size: 1024,
	}))

	_, f, err := mgr.Start("err.bin", 1024)
	require.NoError(t, err)
	defer f.Close()

	err = mgr.WaitForRange("err.bin", 0, 1024)
	assert.Error(t, err, "WaitForRange should propagate download error")
}

func TestManagerCancelIdempotent(t *testing.T) {
	t.Parallel()
	adapter := &testutil.MockRemoteAdapter{}
	mgr, _ := newTestManager(t, adapter, ManagerOptions{})

	// Cancel a path that was never started — should not panic.
	mgr.Cancel("nonexistent.bin")
	mgr.Cancel("nonexistent.bin")
}

// ---------- Phase-4 tests ----------

// TestDownloadNetworkErrorMidStream verifies that a mid-stream I/O error from
// the adapter is propagated via WaitForRange and that the download is removed
// from the manager.
func TestDownloadNetworkErrorMidStream(t *testing.T) {
	t.Parallel()
	adapter := &testutil.MockRemoteAdapter{
		GetFunc: func(_ context.Context, path string, dest io.Writer) error {
			// Write partial content then fail.
			_, _ = dest.Write(bytes.Repeat([]byte("x"), 128))
			return io.ErrUnexpectedEOF
		},
	}
	mgr, cl := newTestManager(t, adapter, ManagerOptions{})

	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path: "partial.bin", State: cache.StateEvicted, Mode: 0100644,
		Size: 1024,
	}))

	_, f, err := mgr.Start("partial.bin", 1024)
	require.NoError(t, err)
	defer f.Close()

	err = mgr.WaitForRange("partial.bin", 0, 1024)
	assert.Error(t, err, "WaitForRange should propagate mid-stream error")
	assert.False(t, mgr.IsDownloading("partial.bin"),
		"download should be removed from manager after error")
}

// TestDownloadContextCancelledOnDone verifies that calling Cancel on an active
// download causes the adapter Get context to be cancelled.
func TestDownloadContextCancelledOnDone(t *testing.T) {
	t.Parallel()

	ctxCancelled := make(chan error, 1)
	started := make(chan struct{})
	adapter := &testutil.MockRemoteAdapter{
		GetFunc: func(ctx context.Context, path string, dest io.Writer) error {
			close(started)
			<-ctx.Done()
			ctxCancelled <- ctx.Err()
			return ctx.Err()
		},
	}
	mgr, cl := newTestManager(t, adapter, ManagerOptions{})

	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path: "cancel-ctx.bin", State: cache.StateEvicted, Mode: 0100644,
		Size: 4096,
	}))

	_, f, err := mgr.Start("cancel-ctx.bin", 4096)
	require.NoError(t, err)
	defer f.Close()

	<-started
	mgr.Cancel("cancel-ctx.bin")

	select {
	case ctxErr := <-ctxCancelled:
		assert.Equal(t, context.Canceled, ctxErr,
			"adapter ctx should be cancelled when download is cancelled")
	case <-time.After(2 * time.Second):
		t.Fatal("adapter Get context was not cancelled after Cancel()")
	}
}

func TestManagerPrefetchIgnoresReadAhead(t *testing.T) {
	t.Parallel()

	const totalSize = int64(3 * 1024 * 1024)
	content := bytes.Repeat([]byte("p"), int(totalSize))

	adapter := &testutil.MockRemoteAdapter{
		GetFunc: func(_ context.Context, path string, dest io.Writer) error {
			_, err := dest.Write(content)
			return err
		},
	}
	mgr, cl := newTestManager(t, adapter, ManagerOptions{ReadAhead: 1 * 1024 * 1024})

	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path: "prefetch.bin", State: cache.StateEvicted, Mode: 0100644,
		Size: totalSize,
	}))

	require.NoError(t, mgr.Prefetch("prefetch.bin", totalSize))

	require.Eventually(t, func() bool {
		entry, err := cl.Stat("prefetch.bin")
		if err != nil || entry == nil || entry.State != cache.StateClean {
			return false
		}

		var rs RangeSet
		if err := json.Unmarshal([]byte(entry.CachedRanges), &rs); err != nil {
			return false
		}
		return rs.IsComplete(totalSize)
	}, 5*time.Second, 20*time.Millisecond, "prefetch should complete full download regardless of read-ahead")

	assert.False(t, mgr.IsDownloading("prefetch.bin"), "prefetch should be removed after completion")
}

func TestManagerCancelledHelperGoroutineIsNotFatal(t *testing.T) {
	t.Parallel()

	const totalSize = int64(10 * 1024 * 1024)

	adapter := &testutil.MockRemoteAdapter{
		GetFunc: func(ctx context.Context, path string, dest io.Writer) error {
			for written := int64(0); written < totalSize; {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}

				chunk := int64(64 * 1024)
				if remaining := totalSize - written; remaining < chunk {
					chunk = remaining
				}
				if _, err := dest.Write(bytes.Repeat([]byte("a"), int(chunk))); err != nil {
					return err
				}
				written += chunk
				time.Sleep(2 * time.Millisecond)
			}
			return nil
		},
		GetRangeFunc: func(ctx context.Context, path string, offset, length int64, dest io.Writer) error {
			for sent := int64(0); sent < length; {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}

				chunk := int64(64 * 1024)
				if remaining := length - sent; remaining < chunk {
					chunk = remaining
				}
				if _, err := dest.Write(bytes.Repeat([]byte("b"), int(chunk))); err != nil {
					return err
				}
				sent += chunk
				time.Sleep(2 * time.Millisecond)
			}
			return nil
		},
	}

	mgr, cl := newTestManager(t, adapter, ManagerOptions{})

	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path: "cancel-helper.bin", State: cache.StateEvicted, Mode: 0100644,
		Size: totalSize,
	}))

	dl, f, err := mgr.Start("cancel-helper.bin", totalSize)
	require.NoError(t, err)
	defer f.Close()

	// Spawn a non-sequential goroutine far ahead of the sequential stream,
	// then force it to be cancelled as far-behind by a much later read.
	mgr.Hint("cancel-helper.bin", 3*1024*1024)
	require.NoError(t, dl.WaitForRange(8*1024*1024, 1))

	// The intentional helper cancellation must not poison the whole download.
	require.NoError(t, dl.WaitForRange(0, totalSize))

	require.Eventually(t, func() bool {
		e, err := cl.Stat("cancel-helper.bin")
		return err == nil && e != nil && e.State == cache.StateClean
	}, 8*time.Second, 20*time.Millisecond, "download should complete and be marked clean")
}
