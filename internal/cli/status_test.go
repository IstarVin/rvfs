package cli

import (
	"bytes"
	"testing"

	"github.com/IstarVin/rvfs/internal/ipc"
	"github.com/stretchr/testify/assert"
)

func TestPrintStatus_ShowsDiskAndLogicalWhenDifferent(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	printStatus(&b, ipc.StatusResponse{
		Source:            "gdrive:Documents",
		Mountpoint:        "/mnt/docs",
		Online:            true,
		CacheUsed:         2 << 30,
		CacheLogicalUsed:  8 << 30,
		CacheTotal:        20 << 30,
		CacheMinFreeSpace: 1 << 30,
		CacheFSFree:       12 << 30,
	}, false)

	out := b.String()
	assert.Contains(t, out, "used on disk")
	assert.Contains(t, out, "logical")
	assert.Contains(t, out, "available to fill")
}

func TestPrintStatus_UsesLegacyCacheTextWhenLogicalUnavailable(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	printStatus(&b, ipc.StatusResponse{
		Source:            "gdrive:Documents",
		Mountpoint:        "/mnt/docs",
		Online:            true,
		CacheUsed:         2 << 30,
		CacheMinFreeSpace: 1 << 30,
		CacheFSFree:       12 << 30,
	}, false)

	out := b.String()
	assert.Contains(t, out, "used, ")
	assert.Contains(t, out, "available to fill")
	assert.NotContains(t, out, "used on disk")
}
