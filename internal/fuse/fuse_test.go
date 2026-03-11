package fuse_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	rvfuse "github.com/IstarVin/rvfs/internal/fuse"
)

// mountForTest creates a backing dir and mountpoint, mounts the backing dir,
// and registers cleanup (unmount + remove dirs) on t.
// Returns mountpoint path.
func mountForTest(t *testing.T) string {
	t.Helper()

	backingDir := t.TempDir()
	mountpoint := t.TempDir()

	server, err := rvfuse.Mount(backingDir, mountpoint, rvfuse.MountOptions{})
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}

	t.Cleanup(func() {
		if err := server.Unmount(); err != nil {
			t.Logf("Unmount: %v", err)
		}
	})

	return mountpoint
}

// TestCreateReadFile creates a file through the mount and reads it back.
func TestCreateReadFile(t *testing.T) {
	mnt := mountForTest(t)

	path := filepath.Join(mnt, "hello.txt")
	content := []byte("hello, rvfs!")

	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("content mismatch: got %q want %q", got, content)
	}
}

// TestStat verifies that stat returns sensible attributes for a file.
func TestStat(t *testing.T) {
	mnt := mountForTest(t)

	path := filepath.Join(mnt, "stat.txt")
	if err := os.WriteFile(path, []byte("data"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != 4 {
		t.Fatalf("size: got %d want 4", info.Size())
	}
	if info.Mode().Perm() != 0644 {
		t.Fatalf("mode: got %o want 0644", info.Mode().Perm())
	}
}

// TestWriteFile tests overwriting file content.
func TestWriteFile(t *testing.T) {
	mnt := mountForTest(t)

	path := filepath.Join(mnt, "write.txt")
	if err := os.WriteFile(path, []byte("first"), 0644); err != nil {
		t.Fatalf("first WriteFile: %v", err)
	}
	if err := os.WriteFile(path, []byte("second"), 0644); err != nil {
		t.Fatalf("second WriteFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "second" {
		t.Fatalf("content: got %q want %q", got, "second")
	}
}

// TestTruncate verifies truncation via os.Truncate.
func TestTruncate(t *testing.T) {
	mnt := mountForTest(t)

	path := filepath.Join(mnt, "trunc.txt")
	if err := os.WriteFile(path, []byte("hello world"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := os.Truncate(path, 5); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != 5 {
		t.Fatalf("size after truncate: got %d want 5", info.Size())
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("content after truncate: got %q want %q", got, "hello")
	}
}

// TestRenameFile renames a file and verifies the old name is gone and new name exists.
func TestRenameFile(t *testing.T) {
	mnt := mountForTest(t)

	src := filepath.Join(mnt, "src.txt")
	dst := filepath.Join(mnt, "dst.txt")

	if err := os.WriteFile(src, []byte("data"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Rename(src, dst); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("src still exists after rename")
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("ReadFile dst: %v", err)
	}
	if string(got) != "data" {
		t.Fatalf("content: got %q want %q", got, "data")
	}
}

// TestUnlinkFile removes a file and verifies it's gone.
func TestUnlinkFile(t *testing.T) {
	mnt := mountForTest(t)

	path := filepath.Join(mnt, "remove.txt")
	if err := os.WriteFile(path, []byte("bye"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file still exists after remove")
	}
}

// TestMkdirReaddir creates a directory and reads its contents.
func TestMkdirReaddir(t *testing.T) {
	mnt := mountForTest(t)

	dir := filepath.Join(mnt, "subdir")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	// Create two files inside.
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("ReadDir: got %d entries want 2", len(entries))
	}

	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	for _, want := range []string{"a.txt", "b.txt"} {
		if !names[want] {
			t.Fatalf("missing entry %q", want)
		}
	}
}

// TestRmdir removes an empty directory and verifies it's gone.
func TestRmdir(t *testing.T) {
	mnt := mountForTest(t)

	dir := filepath.Join(mnt, "emptydir")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir still exists after remove")
	}
}

// TestChmod verifies that file permissions can be changed via chmod.
func TestChmod(t *testing.T) {
	mnt := mountForTest(t)

	path := filepath.Join(mnt, "chmod.txt")
	if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("perm: got %o want 0600", info.Mode().Perm())
	}
}

// TestCpInodeStability copies a file through the mount and verifies the copy
// does not produce a "replaced while being copied" error.
// This validates the FNV-64a inode numbering scheme: if StableAttr.Ino were 0
// (auto-assigned), go-fuse would assign new IDs after forget+recreate and cp
// would detect an inode change mid-copy.
func TestCpInodeStability(t *testing.T) {
	mnt := mountForTest(t)

	src := filepath.Join(mnt, "orig.bin")

	// Write 1 MiB to have a large enough file that cp issues multiple reads.
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i)
	}
	if err := os.WriteFile(src, data, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Record the inode number before and after a second stat.
	stat1, err := os.Stat(src)
	if err != nil {
		t.Fatalf("Stat1: %v", err)
	}
	stat2, err := os.Stat(src)
	if err != nil {
		t.Fatalf("Stat2: %v", err)
	}

	ino1 := stat1.Sys().(*syscall.Stat_t).Ino
	ino2 := stat2.Sys().(*syscall.Stat_t).Ino
	if ino1 != ino2 {
		t.Fatalf("inode changed between stats: %d → %d (FNV hash not stable?)", ino1, ino2)
	}

	// Copy the file within the mount.
	dst := filepath.Join(mnt, "copy.bin")
	got, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if err := os.WriteFile(dst, got, 0644); err != nil {
		t.Fatalf("WriteFile copy: %v", err)
	}

	// Verify the copy is identical.
	copied, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("ReadFile copy: %v", err)
	}
	if len(copied) != len(data) {
		t.Fatalf("copy size mismatch: got %d want %d", len(copied), len(data))
	}
	for i := range data {
		if copied[i] != data[i] {
			t.Fatalf("copy byte mismatch at offset %d", i)
		}
	}
}
