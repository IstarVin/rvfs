package cli

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/download"
	"github.com/IstarVin/rvfs/internal/ipc"
	"github.com/IstarVin/rvfs/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestMountHandler(t *testing.T, adapter *testutil.MockRemoteAdapter) (*mountHandler, *download.Manager, *cache.CacheLayer) {
	t.Helper()

	cl, err := cache.NewCacheLayer(t.TempDir(), "test-remote")
	require.NoError(t, err)
	t.Cleanup(func() { cl.Close() })

	mgr := download.NewManager(adapter, cl, nil, download.ManagerOptions{})
	h := &mountHandler{
		cl:          cl,
		downloadMgr: mgr,
		prefetchQ:   make(chan prefetchRequest, 32),
	}
	return h, mgr, cl
}

func putPrefetchEntry(t *testing.T, cl *cache.CacheLayer, path string, size int64) {
	t.Helper()
	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path:  path,
		Mode:  0100644,
		Size:  size,
		State: cache.StateEvicted,
	}))
}

func TestMountHandlerSequentialPrefetchQueueIsOneAtATime(t *testing.T) {
	t.Parallel()

	var active atomic.Int32
	var maxActive atomic.Int32
	started := make(chan string, 8)
	permits := map[string]chan struct{}{
		"a.bin": make(chan struct{}, 1),
		"b.bin": make(chan struct{}, 1),
		"c.bin": make(chan struct{}, 1),
	}

	adapter := &testutil.MockRemoteAdapter{
		GetFunc: func(ctx context.Context, path string, dest io.Writer) error {
			cur := active.Add(1)
			for {
				prev := maxActive.Load()
				if cur <= prev || maxActive.CompareAndSwap(prev, cur) {
					break
				}
			}
			defer active.Add(-1)

			started <- path
			select {
			case <-permits[path]:
			case <-ctx.Done():
				return ctx.Err()
			}
			_, err := dest.Write([]byte("data"))
			return err
		},
	}

	h, mgr, cl := newTestMountHandler(t, adapter)
	for _, p := range []string{"a.bin", "b.bin", "c.bin"} {
		putPrefetchEntry(t, cl, p, 4)
	}

	h.startPrefetchWorker()
	t.Cleanup(h.stopPrefetchWorker)

	require.NoError(t, h.HandlePrefetch("a.bin", true))
	require.NoError(t, h.HandlePrefetch("b.bin", true))
	require.NoError(t, h.HandlePrefetch("c.bin", true))

	waitStarted := func(want string) {
		t.Helper()
		select {
		case got := <-started:
			require.Equal(t, want, got)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s to start", want)
		}
	}
	assertNotStartedSoon := func() {
		t.Helper()
		select {
		case got := <-started:
			t.Fatalf("unexpected concurrent start for %s", got)
		case <-time.After(120 * time.Millisecond):
		}
	}

	waitStarted("a.bin")
	assertNotStartedSoon()
	permits["a.bin"] <- struct{}{}

	waitStarted("b.bin")
	assertNotStartedSoon()
	permits["b.bin"] <- struct{}{}

	waitStarted("c.bin")
	permits["c.bin"] <- struct{}{}

	require.Eventually(t, func() bool {
		return !mgr.IsDownloading("a.bin") && !mgr.IsDownloading("b.bin") && !mgr.IsDownloading("c.bin")
	}, 3*time.Second, 20*time.Millisecond)

	assert.Equal(t, int32(1), maxActive.Load(), "sequential queue should cap active downloads at 1")
}

func TestMountHandlerNonSequentialPrefetchBypassesQueue(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	adapter := &testutil.MockRemoteAdapter{
		GetFunc: func(_ context.Context, _ string, dest io.Writer) error {
			calls.Add(1)
			_, err := dest.Write([]byte("data"))
			return err
		},
	}

	h, mgr, cl := newTestMountHandler(t, adapter)
	putPrefetchEntry(t, cl, "one.bin", 4)

	require.NoError(t, h.HandlePrefetch("one.bin", false))
	require.Eventually(t, func() bool {
		return calls.Load() == 1 && !mgr.IsDownloading("one.bin")
	}, 2*time.Second, 20*time.Millisecond)
}

func TestMountHandlerSequentialPrefetchRequiresQueue(t *testing.T) {
	t.Parallel()

	adapter := &testutil.MockRemoteAdapter{}
	cl, err := cache.NewCacheLayer(t.TempDir(), "test-remote")
	require.NoError(t, err)
	t.Cleanup(func() { cl.Close() })

	mgr := download.NewManager(adapter, cl, nil, download.ManagerOptions{})
	h := &mountHandler{cl: cl, downloadMgr: mgr}
	putPrefetchEntry(t, cl, "x.bin", 4)

	err = h.HandlePrefetch("x.bin", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prefetch queue unavailable")
}

func TestMountHandlerStopPrefetchWorkerIsIdempotent(t *testing.T) {
	t.Parallel()

	adapter := &testutil.MockRemoteAdapter{}
	h, _, _ := newTestMountHandler(t, adapter)

	h.startPrefetchWorker()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		h.stopPrefetchWorker()
	}()
	go func() {
		defer wg.Done()
		h.stopPrefetchWorker()
	}()
	wg.Wait()
}

func TestMountHandlerDownloadsIncludesQueuedSequentialPrefetch(t *testing.T) {
	t.Parallel()

	started := make(chan string, 4)
	release := make(chan struct{}, 3)
	adapter := &testutil.MockRemoteAdapter{
		GetFunc: func(ctx context.Context, path string, dest io.Writer) error {
			started <- path
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			_, err := dest.Write([]byte("data"))
			return err
		},
	}

	h, mgr, cl := newTestMountHandler(t, adapter)
	for _, p := range []string{"a.bin", "b.bin", "c.bin"} {
		putPrefetchEntry(t, cl, p, 4)
	}

	h.startPrefetchWorker()
	t.Cleanup(h.stopPrefetchWorker)

	require.NoError(t, h.HandlePrefetch("a.bin", true))
	require.NoError(t, h.HandlePrefetch("b.bin", true))
	require.NoError(t, h.HandlePrefetch("c.bin", true))

	select {
	case got := <-started:
		require.Equal(t, "a.bin", got)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first prefetch to start")
	}

	resp, err := h.HandleDownloads("")
	require.NoError(t, err)

	statesByPath := make(map[string]string, len(resp.Entries))
	for _, e := range resp.Entries {
		statesByPath[e.Path] = e.State
	}
	require.Equal(t, "downloading", statesByPath["a.bin"])
	require.Equal(t, "queued", statesByPath["b.bin"])
	require.Equal(t, "queued", statesByPath["c.bin"])

	queued := make([]ipc.DownloadStatusEntry, 0, 2)
	for _, e := range resp.Entries {
		if e.State == "queued" {
			queued = append(queued, e)
		}
	}
	require.Len(t, queued, 2)
	assert.Equal(t, "b.bin", queued[0].Path)
	assert.Equal(t, "c.bin", queued[1].Path)

	release <- struct{}{}
	release <- struct{}{}
	release <- struct{}{}
	require.Eventually(t, func() bool {
		return !mgr.IsDownloading("a.bin") && !mgr.IsDownloading("b.bin") && !mgr.IsDownloading("c.bin")
	}, 3*time.Second, 20*time.Millisecond)
}

func TestMountHandlerDownloadsQueuedPathFilter(t *testing.T) {
	t.Parallel()

	started := make(chan string, 4)
	release := make(chan struct{}, 3)
	adapter := &testutil.MockRemoteAdapter{
		GetFunc: func(ctx context.Context, path string, dest io.Writer) error {
			started <- path
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			_, err := dest.Write([]byte("data"))
			return err
		},
	}

	h, mgr, cl := newTestMountHandler(t, adapter)
	for _, p := range []string{"a.bin", "b.bin", "c.bin"} {
		putPrefetchEntry(t, cl, p, 4)
	}

	h.startPrefetchWorker()
	t.Cleanup(h.stopPrefetchWorker)

	require.NoError(t, h.HandlePrefetch("a.bin", true))
	require.NoError(t, h.HandlePrefetch("b.bin", true))
	require.NoError(t, h.HandlePrefetch("c.bin", true))

	select {
	case got := <-started:
		require.Equal(t, "a.bin", got)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first prefetch to start")
	}

	resp, err := h.HandleDownloads("c.bin")
	require.NoError(t, err)
	require.Len(t, resp.Entries, 1)
	assert.Equal(t, "c.bin", resp.Entries[0].Path)
	assert.Equal(t, "queued", resp.Entries[0].State)

	release <- struct{}{}
	release <- struct{}{}
	release <- struct{}{}
	require.Eventually(t, func() bool {
		return !mgr.IsDownloading("a.bin") && !mgr.IsDownloading("b.bin") && !mgr.IsDownloading("c.bin")
	}, 3*time.Second, 20*time.Millisecond)
}
