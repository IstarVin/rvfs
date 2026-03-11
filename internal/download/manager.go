package download

import (
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/remote"
)

const (
	// chunkSize is the size of chunks read from the remote during download.
	chunkSize = 256 * 1024 // 256 KiB

	// seekThreshold is the minimum gap between the sequential download
	// position and a requested offset before we spawn a range goroutine.
	seekThreshold = 1 * 1024 * 1024 // 1 MiB
)

// Manager manages all in-progress remote→cache downloads.
type Manager struct {
	adapter remote.RemoteAdapter
	cache   *cache.CacheLayer

	mu        sync.Mutex
	downloads map[string]*Download
}

// NewManager creates a Download Manager.
func NewManager(adapter remote.RemoteAdapter, cl *cache.CacheLayer) *Manager {
	return &Manager{
		adapter:   adapter,
		cache:     cl,
		downloads: make(map[string]*Download),
	}
}

// waiter represents a goroutine blocked waiting for a byte range.
type waiter struct {
	offset int64
	size   int64
	ch     chan error
}

// Download tracks the state of a single file being downloaded.
type Download struct {
	path      string
	cacheFile *os.File
	rangeSet  *RangeSet
	totalSize int64

	mu      sync.Mutex
	cond    *sync.Cond
	waiters []waiter
	seqPos  int64 // position of the sequential download goroutine
	err     error // first fatal error
	done    bool

	goroutines map[int64]func() // startOffset → cancel function
	mgr        *Manager
}

// Start begins downloading a remote file into the cache. If a download for
// the same path is already in progress, returns the existing Download (dedup).
// The returned *os.File is the cache file open for reading.
func (m *Manager) Start(path string, totalSize int64) (*Download, *os.File, error) {
	m.mu.Lock()
	if dl, ok := m.downloads[path]; ok {
		m.mu.Unlock()
		// Open a separate read fd for this caller.
		f, err := m.cache.Open(path, os.O_RDONLY)
		if err != nil {
			return nil, nil, err
		}
		return dl, f, nil
	}
	m.mu.Unlock()

	// Create sparse cache file.
	cacheFile, err := m.cache.OpenOrCreate(path, totalSize)
	if err != nil {
		return nil, nil, fmt.Errorf("create cache file for %q: %w", path, err)
	}

	dl := &Download{
		path:       path,
		cacheFile:  cacheFile,
		rangeSet:   NewRangeSet(),
		totalSize:  totalSize,
		goroutines: make(map[int64]func()),
		mgr:        m,
	}
	dl.cond = sync.NewCond(&dl.mu)

	m.mu.Lock()
	// Double-check after releasing/reacquiring lock.
	if existing, ok := m.downloads[path]; ok {
		m.mu.Unlock()
		cacheFile.Close()
		f, err := m.cache.Open(path, os.O_RDONLY)
		if err != nil {
			return nil, nil, err
		}
		return existing, f, nil
	}
	m.downloads[path] = dl
	m.mu.Unlock()

	// Mark as downloading in DB.
	_ = m.cache.DB().SetState(path, cache.StateDownloading)

	// Spawn sequential download goroutine from offset 0.
	dl.spawnGoroutine(0, true)

	// Open separate read fd for the caller.
	readFile, err := m.cache.Open(path, os.O_RDONLY)
	if err != nil {
		return nil, nil, err
	}
	return dl, readFile, nil
}

// WaitForRange blocks until [offset, offset+size) is available, or returns
// an error if the download fails.
func (m *Manager) WaitForRange(path string, offset, size int64) error {
	m.mu.Lock()
	dl, ok := m.downloads[path]
	m.mu.Unlock()

	if !ok {
		// No active download — data should already be on disk (state clean/dirty).
		return nil
	}

	return dl.waitForRange(offset, size)
}

// Cancel cancels all goroutines for a download and removes it from the manager.
func (m *Manager) Cancel(path string) {
	m.mu.Lock()
	dl, ok := m.downloads[path]
	if ok {
		delete(m.downloads, path)
	}
	m.mu.Unlock()

	if ok {
		dl.cancel()
	}
}

// IsDownloading returns true if a download is in progress for path.
func (m *Manager) IsDownloading(path string) bool {
	m.mu.Lock()
	_, ok := m.downloads[path]
	m.mu.Unlock()
	return ok
}

// ---------- Download methods ----------

func (dl *Download) waitForRange(offset, size int64) error {
	dl.mu.Lock()
	defer dl.mu.Unlock()

	// Fast path: already available.
	if dl.rangeSet.Contains(offset, size) {
		return nil
	}

	// If download already errored, return the error.
	if dl.err != nil {
		return dl.err
	}

	// If the sequential goroutine is far behind the requested offset,
	// spawn a range goroutine to jump ahead.
	if offset-dl.seqPos > seekThreshold {
		dl.mu.Unlock()
		dl.maybeSpawnRange(offset)
		dl.mu.Lock()
	}

	// Wait until the range is covered or an error occurs.
	for !dl.rangeSet.Contains(offset, size) && dl.err == nil {
		dl.cond.Wait()
	}

	return dl.err
}

func (dl *Download) maybeSpawnRange(offset int64) {
	dl.mu.Lock()
	defer dl.mu.Unlock()

	// Don't spawn if already have a goroutine covering this area.
	for startOff := range dl.goroutines {
		if startOff <= offset && offset-startOff < seekThreshold {
			return
		}
	}

	dl.spawnGoroutine(offset, false)
}

func (dl *Download) spawnGoroutine(startOffset int64, isSequential bool) {
	// Use a done channel as the cancellation mechanism.
	doneCh := make(chan struct{})
	dl.goroutines[startOffset] = func() { close(doneCh) }

	go dl.downloadLoop(startOffset, isSequential, doneCh)
}

func (dl *Download) downloadLoop(startOffset int64, isSequential bool, doneCh chan struct{}) {
	defer func() {
		dl.mu.Lock()
		delete(dl.goroutines, startOffset)
		// If this was the last goroutine and we're complete, mark done.
		if dl.rangeSet.IsComplete(dl.totalSize) && !dl.done {
			dl.done = true
			dl.cond.Broadcast()
			// Mark clean in DB and remove from manager.
			go dl.finish()
		}
		dl.mu.Unlock()
	}()

	offset := startOffset
	remaining := dl.totalSize - offset
	if remaining <= 0 {
		return
	}

	pr, pw := io.Pipe()

	go func() {
		var err error
		if isSequential && startOffset == 0 {
			err = dl.mgr.adapter.Get(dl.path, pw)
		} else {
			length := dl.totalSize - startOffset
			err = dl.mgr.adapter.GetRange(dl.path, startOffset, length, pw)
		}
		pw.CloseWithError(err)
	}()

	buf := make([]byte, chunkSize)
	pos := offset

	for {
		select {
		case <-doneCh:
			pr.Close()
			return
		default:
		}

		n, err := pr.Read(buf)
		if n > 0 {
			// Write chunk to cache file at correct offset.
			if _, werr := dl.cacheFile.WriteAt(buf[:n], pos); werr != nil {
				dl.mu.Lock()
				if dl.err == nil {
					dl.err = fmt.Errorf("write cache: %w", werr)
				}
				dl.cond.Broadcast()
				dl.mu.Unlock()
				pr.Close()
				return
			}

			dl.mu.Lock()
			dl.rangeSet.Add(pos, int64(n))
			if isSequential {
				dl.seqPos = pos + int64(n)
			}
			dl.cond.Broadcast()
			dl.mu.Unlock()

			pos += int64(n)
		}

		if err != nil {
			if err != io.EOF {
				dl.mu.Lock()
				if dl.err == nil {
					dl.err = fmt.Errorf("download %q at offset %d: %w", dl.path, startOffset, err)
				}
				dl.cond.Broadcast()
				dl.mu.Unlock()
			}
			pr.Close()
			return
		}
	}
}

func (dl *Download) finish() {
	dl.cacheFile.Close()

	// Update DB: set state clean, update cached_ranges.
	rangesJSON, _ := dl.rangeSet.MarshalJSON()
	entry, err := dl.mgr.cache.Stat(dl.path)
	if err == nil && entry != nil {
		entry.State = cache.StateClean
		entry.CachedRanges = string(rangesJSON)
		_ = dl.mgr.cache.DB().PutFile(entry)
	}

	dl.mgr.mu.Lock()
	delete(dl.mgr.downloads, dl.path)
	dl.mgr.mu.Unlock()
}

func (dl *Download) cancel() {
	dl.mu.Lock()
	for _, cancelFn := range dl.goroutines {
		cancelFn()
	}
	dl.err = fmt.Errorf("download cancelled")
	dl.cond.Broadcast()
	dl.mu.Unlock()

	dl.cacheFile.Close()
}
