package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/config"
	"github.com/IstarVin/rvfs/internal/ipc"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type queueTestHandler struct {
	uploads ipc.UploadStatusResponse
}

func (h *queueTestHandler) HandleStatus() (ipc.StatusResponse, error) {
	return ipc.StatusResponse{}, nil
}

func (h *queueTestHandler) HandleSync(force bool) error {
	return nil
}

func (h *queueTestHandler) HandlePrefetch(path string, sequential bool) error {
	return nil
}

func (h *queueTestHandler) HandleEvict(path string) error {
	return nil
}

func (h *queueTestHandler) HandleDownloads(path string) (ipc.DownloadStatusResponse, error) {
	return ipc.DownloadStatusResponse{}, nil
}

func (h *queueTestHandler) HandleUploads(path string) (ipc.UploadStatusResponse, error) {
	return h.uploads, nil
}

func TestQueueCommandShowsUploadProgress(t *testing.T) {
	prevCfg := globalCfg
	prevQueueJSON := queueJSON
	prevRuntime := os.Getenv("XDG_RUNTIME_DIR")
	t.Cleanup(func() {
		globalCfg = prevCfg
		queueJSON = prevQueueJSON
		require.NoError(t, os.Setenv("XDG_RUNTIME_DIR", prevRuntime))
	})

	cacheDir := t.TempDir()
	runtimeDir := t.TempDir()
	require.NoError(t, os.Setenv("XDG_RUNTIME_DIR", runtimeDir))
	globalCfg = &config.Config{Mount: config.MountConfig{CacheDir: cacheDir}}
	queueJSON = false

	cl, err := cache.NewCacheLayer(cacheDir, "demo")
	require.NoError(t, err)
	t.Cleanup(func() { _ = cl.Close() })

	f, _, err := cl.Create("upload.bin", 0644)
	require.NoError(t, err)
	_, err = f.Write([]byte("1234567890"))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.NoError(t, cl.DB().AddPendingOp(&cache.PendingOp{Op: "put", Path: "upload.bin", QueuedAt: time.Now().Add(-time.Minute).Unix()}))

	h := &queueTestHandler{uploads: ipc.UploadStatusResponse{Entries: []ipc.UploadStatusEntry{{
		Path:      "upload.bin",
		State:     "uploading",
		Uploaded:  5,
		TotalSize: 10,
		StartedAt: time.Now().Add(-2 * time.Second).Unix(),
	}}}}
	srv := ipc.NewServer(ipc.SockPath("demo"), h)
	require.NoError(t, srv.Listen())
	t.Cleanup(srv.Close)

	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetOut(out)

	err = queueCmd.RunE(cmd, []string{"demo:"})
	require.NoError(t, err)

	assert.Contains(t, out.String(), "uploading")
	assert.Contains(t, out.String(), "50%")
	assert.Contains(t, out.String(), "5 B / 10 B")
	assert.Contains(t, out.String(), "1 item uploading")
}

func TestQueueCommandJSONIncludesUploadProgress(t *testing.T) {
	prevCfg := globalCfg
	prevQueueJSON := queueJSON
	t.Cleanup(func() {
		globalCfg = prevCfg
		queueJSON = prevQueueJSON
	})

	cacheDir := t.TempDir()
	globalCfg = &config.Config{Mount: config.MountConfig{CacheDir: cacheDir}}
	queueJSON = true

	cl, err := cache.NewCacheLayer(cacheDir, "demo")
	require.NoError(t, err)
	t.Cleanup(func() { _ = cl.Close() })

	require.NoError(t, cl.DB().AddPendingOp(&cache.PendingOp{Op: "put", Path: "upload.bin", QueuedAt: time.Now().Unix()}))
	view, _, _, _ := buildQueueView("demo:", cl.DB(), []*cache.PendingOp{{Op: "put", Path: "upload.bin", QueuedAt: time.Now().Unix()}}, map[string]ipc.UploadStatusEntry{
		"upload.bin": {
			Path:      "upload.bin",
			State:     "uploading",
			Uploaded:  5,
			TotalSize: 10,
			StartedAt: 123,
		},
	})

	buf := &bytes.Buffer{}
	require.NoError(t, writeJSON(buf, view))

	var decoded queueView
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	require.Len(t, decoded.Operations, 1)
	assert.Equal(t, "uploading", decoded.Operations[0].Status)
	assert.Equal(t, int64(5), decoded.Operations[0].UploadedBytes)
	assert.Equal(t, 50.0, decoded.Operations[0].ProgressPercent)
	assert.Equal(t, int64(123), decoded.Operations[0].StartedAt)
	assert.Equal(t, int64(10), decoded.Operations[0].Size)
}
