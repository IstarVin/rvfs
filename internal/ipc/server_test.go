package ipc

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockHandler is a test double for the Handler interface.
type mockHandler struct {
	mu            sync.Mutex
	statusResp    StatusResponse
	statusErr     error
	syncErr       error
	syncForceSeen bool
	prefetchErr   error
	prefetchSeq   bool
	evictErr      error
	downloadsResp DownloadStatusResponse
	downloadsErr  error
	lastPath      string
}

func (m *mockHandler) HandleStatus() (StatusResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusResp, m.statusErr
}

func (m *mockHandler) HandleSync(force bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncForceSeen = force
	return m.syncErr
}

func (m *mockHandler) HandlePrefetch(path string, sequential bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastPath = path
	m.prefetchSeq = sequential
	return m.prefetchErr
}

func (m *mockHandler) HandleEvict(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastPath = path
	return m.evictErr
}

func (m *mockHandler) HandleDownloads(path string) (DownloadStatusResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastPath = path
	return m.downloadsResp, m.downloadsErr
}

// startTestServer starts a Server with the given handler, registers cleanup,
// and returns the socket path.
func startTestServer(t *testing.T, h Handler) string {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(sockPath, h)
	require.NoError(t, srv.Listen())
	t.Cleanup(srv.Close)
	return sockPath
}

// ---------- Status ----------

func TestServerStatusHappyPath(t *testing.T) {
	t.Parallel()
	want := StatusResponse{
		Source:     "gdrive:Documents",
		Mountpoint: "/mnt/docs",
		Online:     true,
		CacheUsed:  1024,
		CacheTotal: 4096,
		Pending:    3,
		Conflicts:  1,
	}
	h := &mockHandler{statusResp: want}
	sockPath := startTestServer(t, h)

	c, err := Dial(sockPath)
	require.NoError(t, err)
	defer c.Close()

	got, err := c.Status()
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestServerStatusError(t *testing.T) {
	t.Parallel()
	h := &mockHandler{statusErr: errors.New("db unavailable")}
	sockPath := startTestServer(t, h)

	c, err := Dial(sockPath)
	require.NoError(t, err)
	defer c.Close()

	// The client receives an {"error":"..."} JSON object, which it can't
	// decode into a StatusResponse — it should return an error.
	_, err = c.Status()
	assert.Error(t, err)
}

// ---------- Sync ----------

func TestServerSyncForce(t *testing.T) {
	t.Parallel()
	h := &mockHandler{}
	sockPath := startTestServer(t, h)

	c, err := Dial(sockPath)
	require.NoError(t, err)
	defer c.Close()

	require.NoError(t, c.Sync(true))
	h.mu.Lock()
	assert.True(t, h.syncForceSeen)
	h.mu.Unlock()
}

func TestServerSyncNoForce(t *testing.T) {
	t.Parallel()
	h := &mockHandler{}
	sockPath := startTestServer(t, h)

	c, err := Dial(sockPath)
	require.NoError(t, err)
	defer c.Close()

	require.NoError(t, c.Sync(false))
	h.mu.Lock()
	assert.False(t, h.syncForceSeen)
	h.mu.Unlock()
}

func TestServerSyncError(t *testing.T) {
	t.Parallel()
	h := &mockHandler{syncErr: errors.New("remote unreachable")}
	sockPath := startTestServer(t, h)

	c, err := Dial(sockPath)
	require.NoError(t, err)
	defer c.Close()

	err = c.Sync(false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "remote unreachable")
}

func TestServerPrefetchHappyPath(t *testing.T) {
	t.Parallel()
	h := &mockHandler{}
	sockPath := startTestServer(t, h)

	c, err := Dial(sockPath)
	require.NoError(t, err)
	defer c.Close()

	require.NoError(t, c.Prefetch("docs/a.txt"))
	h.mu.Lock()
	assert.Equal(t, "docs/a.txt", h.lastPath)
	assert.False(t, h.prefetchSeq)
	h.mu.Unlock()
}

func TestServerPrefetchSequentialHappyPath(t *testing.T) {
	t.Parallel()
	h := &mockHandler{}
	sockPath := startTestServer(t, h)

	c, err := Dial(sockPath)
	require.NoError(t, err)
	defer c.Close()

	require.NoError(t, c.PrefetchSequential("docs/a.txt"))
	h.mu.Lock()
	assert.Equal(t, "docs/a.txt", h.lastPath)
	assert.True(t, h.prefetchSeq)
	h.mu.Unlock()
}

func TestServerPrefetchError(t *testing.T) {
	t.Parallel()
	h := &mockHandler{prefetchErr: errors.New("not found")}
	sockPath := startTestServer(t, h)

	c, err := Dial(sockPath)
	require.NoError(t, err)
	defer c.Close()

	err = c.Prefetch("docs/missing.txt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestServerEvictHappyPath(t *testing.T) {
	t.Parallel()
	h := &mockHandler{}
	sockPath := startTestServer(t, h)

	c, err := Dial(sockPath)
	require.NoError(t, err)
	defer c.Close()

	require.NoError(t, c.Evict("docs/a.txt"))
	h.mu.Lock()
	assert.Equal(t, "docs/a.txt", h.lastPath)
	h.mu.Unlock()
}

func TestServerDownloadsHappyPath(t *testing.T) {
	t.Parallel()
	h := &mockHandler{downloadsResp: DownloadStatusResponse{Entries: []DownloadStatusEntry{{
		Path:       "docs/a.txt",
		State:      "downloading",
		Downloaded: 5,
		TotalSize:  10,
	}}}}
	sockPath := startTestServer(t, h)

	c, err := Dial(sockPath)
	require.NoError(t, err)
	defer c.Close()

	resp, err := c.Downloads("docs/a.txt")
	require.NoError(t, err)
	require.Len(t, resp.Entries, 1)
	assert.Equal(t, "docs/a.txt", resp.Entries[0].Path)
	h.mu.Lock()
	assert.Equal(t, "docs/a.txt", h.lastPath)
	h.mu.Unlock()
}

func TestServerDownloadsError(t *testing.T) {
	t.Parallel()
	h := &mockHandler{downloadsErr: errors.New("db unavailable")}
	sockPath := startTestServer(t, h)

	c, err := Dial(sockPath)
	require.NoError(t, err)
	defer c.Close()

	_, err = c.Downloads("docs/a.txt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db unavailable")
}

// ---------- Protocol edge cases ----------

func TestServerUnknownCommand(t *testing.T) {
	t.Parallel()
	h := &mockHandler{}
	sockPath := startTestServer(t, h)

	conn, err := net.Dial("unix", sockPath)
	require.NoError(t, err)
	defer conn.Close()

	_, err = conn.Write([]byte(`{"cmd":"bogus"}` + "\n"))
	require.NoError(t, err)

	scanner := bufio.NewScanner(conn)
	require.True(t, scanner.Scan())

	var resp map[string]string
	require.NoError(t, json.Unmarshal(scanner.Bytes(), &resp))
	assert.Contains(t, resp, "error")
}

func TestServerBadJSON(t *testing.T) {
	t.Parallel()
	h := &mockHandler{}
	sockPath := startTestServer(t, h)

	conn, err := net.Dial("unix", sockPath)
	require.NoError(t, err)
	defer conn.Close()

	_, err = conn.Write([]byte("not valid json\n"))
	require.NoError(t, err)

	scanner := bufio.NewScanner(conn)
	require.True(t, scanner.Scan(), "server should respond before closing")

	var resp map[string]string
	require.NoError(t, json.Unmarshal(scanner.Bytes(), &resp))
	assert.Contains(t, resp, "error")
}

// ---------- Concurrency ----------

func TestServerConcurrentConns(t *testing.T) {
	t.Parallel()
	want := StatusResponse{Source: "gdrive:Photos", Online: true}
	h := &mockHandler{statusResp: want}
	sockPath := startTestServer(t, h)

	var wg sync.WaitGroup
	const n = 8
	errs := make([]error, n)
	resps := make([]StatusResponse, n)

	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			c, dialErr := Dial(sockPath)
			if dialErr != nil {
				errs[idx] = dialErr
				return
			}
			defer c.Close()
			resps[idx], errs[idx] = c.Status()
		}(i)
	}
	wg.Wait()

	for i := range n {
		require.NoError(t, errs[i], "goroutine %d", i)
		assert.Equal(t, want, resps[i], "goroutine %d", i)
	}
}

// ---------- Lifecycle ----------

func TestServerCloseRemovesSocket(t *testing.T) {
	t.Parallel()
	h := &mockHandler{}
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(sockPath, h)
	require.NoError(t, srv.Listen())

	_, err := os.Stat(sockPath)
	require.NoError(t, err, "socket should exist after Listen")

	srv.Close()

	_, err = os.Stat(sockPath)
	assert.True(t, os.IsNotExist(err), "socket should be removed after Close")
}

func TestServerCloseIdempotent(t *testing.T) {
	t.Parallel()
	h := &mockHandler{}
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(sockPath, h)
	require.NoError(t, srv.Listen())

	assert.NotPanics(t, func() {
		srv.Close()
		srv.Close()
	})
}
