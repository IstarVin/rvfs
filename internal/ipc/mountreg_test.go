package ipc

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRegistry(t *testing.T) *MountRegistry {
	t.Helper()
	mr, err := openMountRegistryAt(filepath.Join(t.TempDir(), "mounts.db"))
	require.NoError(t, err)
	t.Cleanup(func() { mr.Close() })
	return mr
}

// liveEntry returns a MountEntry with the current process PID (guaranteed alive).
func liveEntry(mountpoint, source string) MountEntry {
	return MountEntry{
		Mountpoint: mountpoint,
		Source:     source,
		RemoteName: "gdrive",
		SockPath:   "/tmp/fake.sock",
		PID:        os.Getpid(),
		MountedAt:  time.Now().Unix(),
	}
}

// deadEntry returns a MountEntry with a PID that is reliably not alive.
func deadEntry(mountpoint, source string) MountEntry {
	e := liveEntry(mountpoint, source)
	e.PID = math.MaxInt32
	return e
}

// ---------- Register + Lookup ----------

func TestMountRegRegisterLookup(t *testing.T) {
	t.Parallel()
	mr := newTestRegistry(t)

	entry := liveEntry("/mnt/docs", "gdrive:Documents")
	require.NoError(t, mr.Register(entry))

	got, alive, err := mr.Lookup("/mnt/docs")
	require.NoError(t, err)
	assert.True(t, alive)
	assert.Equal(t, entry.Mountpoint, got.Mountpoint)
	assert.Equal(t, entry.Source, got.Source)
	assert.Equal(t, entry.RemoteName, got.RemoteName)
	assert.Equal(t, entry.SockPath, got.SockPath)
	assert.Equal(t, entry.PID, got.PID)
}

func TestMountRegLookupMissing(t *testing.T) {
	t.Parallel()
	mr := newTestRegistry(t)

	got, alive, err := mr.Lookup("/mnt/never-registered")
	require.NoError(t, err)
	assert.False(t, alive)
	assert.Equal(t, MountEntry{}, got)
}

func TestMountRegLookupDeadPID(t *testing.T) {
	t.Parallel()
	mr := newTestRegistry(t)

	entry := deadEntry("/mnt/ghost", "gdrive:Ghost")
	require.NoError(t, mr.Register(entry))

	got, alive, err := mr.Lookup("/mnt/ghost")
	require.NoError(t, err)
	assert.False(t, alive)
	assert.Equal(t, MountEntry{}, got)

	// Lookup should have automatically purged it.
	all, err := mr.ListAll()
	require.NoError(t, err)
	for _, e := range all {
		assert.NotEqual(t, "/mnt/ghost", e.Mountpoint, "stale entry should have been purged")
	}
}

// ---------- Deregister ----------

func TestMountRegDeregister(t *testing.T) {
	t.Parallel()
	mr := newTestRegistry(t)

	require.NoError(t, mr.Register(liveEntry("/mnt/docs", "gdrive:Documents")))
	require.NoError(t, mr.Deregister("/mnt/docs"))

	_, alive, err := mr.Lookup("/mnt/docs")
	require.NoError(t, err)
	assert.False(t, alive)
}

func TestMountRegDeregisterMissing(t *testing.T) {
	t.Parallel()
	mr := newTestRegistry(t)

	// Should be a no-op, not an error.
	assert.NoError(t, mr.Deregister("/mnt/nonexistent"))
}

// ---------- ListAll ----------

func TestMountRegListAll(t *testing.T) {
	t.Parallel()
	mr := newTestRegistry(t)

	require.NoError(t, mr.Register(liveEntry("/mnt/a", "gdrive:A")))
	require.NoError(t, mr.Register(liveEntry("/mnt/b", "gdrive:B")))
	require.NoError(t, mr.Register(deadEntry("/mnt/dead", "gdrive:Dead")))

	all, err := mr.ListAll()
	require.NoError(t, err)
	require.Len(t, all, 2, "should only include live entries")

	mountpoints := make([]string, len(all))
	for i, e := range all {
		mountpoints[i] = e.Mountpoint
	}
	assert.ElementsMatch(t, []string{"/mnt/a", "/mnt/b"}, mountpoints)
}

// ---------- ListBySource ----------

func TestMountRegListBySource(t *testing.T) {
	t.Parallel()
	mr := newTestRegistry(t)

	e1 := liveEntry("/mnt/docs", "gdrive:Documents")
	e2 := liveEntry("/mnt/photos", "gdrive:Documents")
	e3 := liveEntry("/mnt/music", "gdrive:Music")

	require.NoError(t, mr.Register(e1))
	require.NoError(t, mr.Register(e2))
	require.NoError(t, mr.Register(e3))

	results, err := mr.ListBySource("gdrive:Documents")
	require.NoError(t, err)
	require.Len(t, results, 2)

	mountpoints := make([]string, len(results))
	for i, e := range results {
		mountpoints[i] = e.Mountpoint
	}
	assert.ElementsMatch(t, []string{"/mnt/docs", "/mnt/photos"}, mountpoints)
}

func TestMountRegListBySourceEmpty(t *testing.T) {
	t.Parallel()
	mr := newTestRegistry(t)

	results, err := mr.ListBySource("gdrive:NoSuchRemote")
	require.NoError(t, err)
	assert.Empty(t, results)
}
