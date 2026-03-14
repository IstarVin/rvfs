package cli

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsMountpointTempDirFalse(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ok, err := isMountpoint(dir)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestIsMountpointRootTrue(t *testing.T) {
	t.Parallel()

	root := "/"
	if runtime.GOOS == "windows" {
		root = filepath.VolumeName(filepath.Clean(t.TempDir())) + `\\`
	}
	ok, err := isMountpoint(root)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestIsMountpointMissingPath(t *testing.T) {
	t.Parallel()

	ok, err := isMountpoint(filepath.Join(t.TempDir(), "does-not-exist"))
	require.Error(t, err)
	assert.False(t, ok)
}
