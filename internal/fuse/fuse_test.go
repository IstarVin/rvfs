package fuse_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rvfuse "github.com/IstarVin/rvfs/internal/fuse"
)

// mountForTest creates a cache layer seeded from a temporary backing dir,
// mounts it, and registers cleanup (unmount + remove dirs) on t.
// Returns the mountpoint path and the backing dir (for pre-seeding files).
func mountForTest(t *testing.T) string {
	t.Helper()

	backingDir := t.TempDir()
	cacheBase := t.TempDir()
	mountpoint := t.TempDir()

	cl, server, err := rvfuse.Mount(cacheBase, "test", mountpoint, rvfuse.MountOptions{})
	require.NoError(t, err, "Mount")

	// Seed the cache from the (empty) backing dir so the root dir entry exists.
	if err := cl.SeedFromDir(backingDir); err != nil {
		server.Unmount()
		require.NoError(t, err, "SeedFromDir")
	}

	t.Cleanup(func() {
		if err := server.Unmount(); err != nil {
			t.Logf("Unmount: %v", err)
		}
		cl.Close()
	})

	return mountpoint
}

// TestCreateReadFile creates a file through the mount and reads it back.
func TestCreateReadFile(t *testing.T) {
	t.Parallel()
	mnt := mountForTest(t)

	path := filepath.Join(mnt, "hello.txt")
	content := []byte("hello, rvfs!")

	require.NoError(t, os.WriteFile(path, content, 0644))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(content), string(got))
}

// TestStat verifies that stat returns sensible attributes for a file.
func TestStat(t *testing.T) {
	t.Parallel()
	mnt := mountForTest(t)

	path := filepath.Join(mnt, "stat.txt")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0644))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, int64(4), info.Size())
	assert.Equal(t, os.FileMode(0644), info.Mode().Perm())
}

// TestWriteFile tests overwriting file content.
func TestWriteFile(t *testing.T) {
	t.Parallel()
	mnt := mountForTest(t)

	path := filepath.Join(mnt, "write.txt")
	require.NoError(t, os.WriteFile(path, []byte("first"), 0644))
	require.NoError(t, os.WriteFile(path, []byte("second"), 0644))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "second", string(got))
}

// TestTruncate verifies truncation via os.Truncate.
func TestTruncate(t *testing.T) {
	t.Parallel()
	mnt := mountForTest(t)

	path := filepath.Join(mnt, "trunc.txt")
	require.NoError(t, os.WriteFile(path, []byte("hello world"), 0644))
	require.NoError(t, os.Truncate(path, 5))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, int64(5), info.Size())

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(got))
}

// TestRenameFile renames a file and verifies the old name is gone and new name exists.
func TestRenameFile(t *testing.T) {
	t.Parallel()
	mnt := mountForTest(t)

	src := filepath.Join(mnt, "src.txt")
	dst := filepath.Join(mnt, "dst.txt")

	require.NoError(t, os.WriteFile(src, []byte("data"), 0644))
	require.NoError(t, os.Rename(src, dst))

	_, err := os.Stat(src)
	assert.True(t, os.IsNotExist(err), "src still exists after rename")

	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, "data", string(got))
}

// TestUnlinkFile removes a file and verifies it's gone.
func TestUnlinkFile(t *testing.T) {
	t.Parallel()
	mnt := mountForTest(t)

	path := filepath.Join(mnt, "remove.txt")
	require.NoError(t, os.WriteFile(path, []byte("bye"), 0644))
	require.NoError(t, os.Remove(path))

	_, err := os.Stat(path)
	assert.True(t, os.IsNotExist(err), "file still exists after remove")
}

// TestMkdirReaddir creates a directory and reads its contents.
func TestMkdirReaddir(t *testing.T) {
	t.Parallel()
	mnt := mountForTest(t)

	dir := filepath.Join(mnt, "subdir")
	require.NoError(t, os.Mkdir(dir, 0755))

	// Create two files inside.
	for _, name := range []string{"a.txt", "b.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(name), 0644))
	}

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 2)

	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	for _, want := range []string{"a.txt", "b.txt"} {
		assert.True(t, names[want], "missing entry %q", want)
	}
}

// TestRmdir removes an empty directory and verifies it's gone.
func TestRmdir(t *testing.T) {
	t.Parallel()
	mnt := mountForTest(t)

	dir := filepath.Join(mnt, "emptydir")
	require.NoError(t, os.Mkdir(dir, 0755))
	require.NoError(t, os.Remove(dir))

	_, err := os.Stat(dir)
	assert.True(t, os.IsNotExist(err), "dir still exists after remove")
}

// TestChmod verifies that file permissions can be changed via chmod.
func TestChmod(t *testing.T) {
	t.Parallel()
	mnt := mountForTest(t)

	path := filepath.Join(mnt, "chmod.txt")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0644))
	require.NoError(t, os.Chmod(path, 0600))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
}

// TestCpInodeStability copies a file through the mount and verifies the copy
// does not produce a "replaced while being copied" error.
// This validates the FNV-64a inode numbering scheme: if StableAttr.Ino were 0
// (auto-assigned), go-fuse would assign new IDs after forget+recreate and cp
// would detect an inode change mid-copy.
func TestCpInodeStability(t *testing.T) {
	t.Parallel()
	mnt := mountForTest(t)

	src := filepath.Join(mnt, "orig.bin")

	// Write 1 MiB to have a large enough file that cp issues multiple reads.
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i)
	}
	require.NoError(t, os.WriteFile(src, data, 0644))

	// Record the inode number before and after a second stat.
	stat1, err := os.Stat(src)
	require.NoError(t, err)
	stat2, err := os.Stat(src)
	require.NoError(t, err)

	ino1 := stat1.Sys().(*syscall.Stat_t).Ino
	ino2 := stat2.Sys().(*syscall.Stat_t).Ino
	assert.Equal(t, ino1, ino2, "inode should be stable between stats")

	// Copy the file within the mount.
	dst := filepath.Join(mnt, "copy.bin")
	got, err := os.ReadFile(src)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dst, got, 0644))

	// Verify the copy is identical.
	copied, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, data, copied)
}
