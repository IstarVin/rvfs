package cli

import (
	"path/filepath"
	"testing"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openCLITestDB(t *testing.T) *cache.MetadataDB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "meta.db")
	db, err := cache.OpenDB(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func putEntry(t *testing.T, db *cache.MetadataDB, path string, isDir bool) {
	t.Helper()
	mode := uint32(0100644)
	if isDir {
		mode = 040755
	}
	require.NoError(t, db.PutFile(&cache.FileEntry{
		Path:  path,
		IsDir: isDir,
		Mode:  mode,
		State: cache.StateClean,
	}))
}

func TestCompactPinnedEntries_CollapsesFullyPinnedDirectory(t *testing.T) {
	t.Parallel()
	db := openCLITestDB(t)

	for _, tc := range []struct {
		path  string
		isDir bool
	}{
		{path: "music", isDir: true},
		{path: "music/a.mp3", isDir: false},
		{path: "music/b.mp3", isDir: false},
		{path: "music/live", isDir: true},
		{path: "music/live/c.mp3", isDir: false},
	} {
		putEntry(t, db, tc.path, tc.isDir)
	}

	require.NoError(t, db.SetPinnedMany([]string{"music", "music/a.mp3", "music/b.mp3", "music/live", "music/live/c.mp3"}, true))

	pins, err := db.ListPinned()
	require.NoError(t, err)
	out, err := compactPinnedEntries(db, pins)
	require.NoError(t, err)

	require.Len(t, out, 1)
	assert.Equal(t, "music", out[0].Path)
	assert.True(t, out[0].IsDir)
}

func TestCompactPinnedEntries_PartialDirectoryKeepsFiles(t *testing.T) {
	t.Parallel()
	db := openCLITestDB(t)

	for _, tc := range []struct {
		path  string
		isDir bool
	}{
		{path: "docs", isDir: true},
		{path: "docs/a.txt", isDir: false},
		{path: "docs/b.txt", isDir: false},
	} {
		putEntry(t, db, tc.path, tc.isDir)
	}

	require.NoError(t, db.SetPinnedMany([]string{"docs/a.txt"}, true))

	pins, err := db.ListPinned()
	require.NoError(t, err)
	out, err := compactPinnedEntries(db, pins)
	require.NoError(t, err)

	require.Len(t, out, 1)
	assert.Equal(t, "docs/a.txt", out[0].Path)
	assert.False(t, out[0].IsDir)
}

func TestRenderPinsTree(t *testing.T) {
	t.Parallel()

	in := []pinnedOutput{
		{Path: "docs", IsDir: true},
		{Path: "videos/trailer.mp4", IsDir: false},
	}
	lines := renderPinsTree(in)

	assert.Equal(t, []string{
		"├── docs/",
		"└── videos/",
		"    └── trailer.mp4",
	}, lines)
}
