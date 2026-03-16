package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/config"
	"github.com/IstarVin/rvfs/internal/ipc"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type downloadsTestHandler struct {
	downloads ipc.DownloadStatusResponse
}

func (h *downloadsTestHandler) HandleStatus() (ipc.StatusResponse, error) {
	return ipc.StatusResponse{}, nil
}

func (h *downloadsTestHandler) HandleSync(force bool) error {
	return nil
}

func (h *downloadsTestHandler) HandlePrefetch(path string, sequential bool) error {
	return nil
}

func (h *downloadsTestHandler) HandleEvict(path string) error {
	return nil
}

func (h *downloadsTestHandler) HandleDownloads(path string) (ipc.DownloadStatusResponse, error) {
	return h.downloads, nil
}

func (h *downloadsTestHandler) HandleUploads(path string) (ipc.UploadStatusResponse, error) {
	return ipc.UploadStatusResponse{}, nil
}

func TestDownloadsCommandShowsQueuedCounts(t *testing.T) {
	prevCfg := globalCfg
	prevDownloadsJSON := downloadsJSON
	prevRuntime := os.Getenv("XDG_RUNTIME_DIR")
	t.Cleanup(func() {
		globalCfg = prevCfg
		downloadsJSON = prevDownloadsJSON
		require.NoError(t, os.Setenv("XDG_RUNTIME_DIR", prevRuntime))
	})

	cacheDir := t.TempDir()
	runtimeDir := t.TempDir()
	require.NoError(t, os.Setenv("XDG_RUNTIME_DIR", runtimeDir))
	globalCfg = &config.Config{Mount: config.MountConfig{CacheDir: cacheDir}}
	downloadsJSON = false

	cl, err := cache.NewCacheLayer(cacheDir, "demo")
	require.NoError(t, err)
	t.Cleanup(func() { _ = cl.Close() })

	h := &downloadsTestHandler{downloads: ipc.DownloadStatusResponse{Entries: []ipc.DownloadStatusEntry{
		{Path: "queued.bin", State: "queued", Downloaded: 0, TotalSize: 10},
		{Path: "active.bin", State: "downloading", Downloaded: 5, TotalSize: 10},
		{Path: "done.bin", State: "complete", Downloaded: 10, TotalSize: 10, Done: true},
		{Path: "broken.bin", State: "error", Err: "network error"},
	}}}
	srv := ipc.NewServer(ipc.SockPath("demo"), h)
	require.NoError(t, srv.Listen())
	t.Cleanup(srv.Close)

	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetOut(out)

	err = downloadsCmd.RunE(cmd, []string{"demo:"})
	require.NoError(t, err)

	printed := out.String()
	assert.Contains(t, printed, "4 tracked, 1 queued, 1 active, 1 complete, 1 with errors")
	assert.Contains(t, printed, "queued.bin")
	assert.Contains(t, printed, "queued")
}

func TestDownloadsCommandJSONIncludesQueuedEntries(t *testing.T) {
	prevCfg := globalCfg
	prevDownloadsJSON := downloadsJSON
	prevRuntime := os.Getenv("XDG_RUNTIME_DIR")
	t.Cleanup(func() {
		globalCfg = prevCfg
		downloadsJSON = prevDownloadsJSON
		require.NoError(t, os.Setenv("XDG_RUNTIME_DIR", prevRuntime))
	})

	cacheDir := t.TempDir()
	runtimeDir := t.TempDir()
	require.NoError(t, os.Setenv("XDG_RUNTIME_DIR", runtimeDir))
	globalCfg = &config.Config{Mount: config.MountConfig{CacheDir: cacheDir}}
	downloadsJSON = true

	cl, err := cache.NewCacheLayer(cacheDir, "demo")
	require.NoError(t, err)
	t.Cleanup(func() { _ = cl.Close() })

	h := &downloadsTestHandler{downloads: ipc.DownloadStatusResponse{Entries: []ipc.DownloadStatusEntry{{
		Path:       "queued.bin",
		State:      "queued",
		Downloaded: 0,
		TotalSize:  10,
	}}}}
	srv := ipc.NewServer(ipc.SockPath("demo"), h)
	require.NoError(t, srv.Listen())
	t.Cleanup(srv.Close)

	buf := &bytes.Buffer{}
	cmd := &cobra.Command{}
	cmd.SetOut(buf)

	err = downloadsCmd.RunE(cmd, []string{"demo:"})
	require.NoError(t, err)

	var decoded ipc.DownloadStatusResponse
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	require.Len(t, decoded.Entries, 1)
	assert.Equal(t, "queued", decoded.Entries[0].State)
	assert.Equal(t, int64(10), decoded.Entries[0].TotalSize)
}
