package download

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/connectivity"
	"github.com/IstarVin/rvfs/internal/remote"
)

const (
	// chunkSize is the size of chunks read from the remote during download.
	chunkSize = 1 * 1024 * 1024 // 1 MiB

	// goroutineWindow is the distance within which we consider a goroutine
	// "close enough" to cover a requested offset — no new goroutine needed.
	goroutineWindow = 2 * 1024 * 1024 // 2 MiB

	// persistInterval is how often we flush CachedRanges to the DB.
	persistInterval = 5 * time.Second

	// persistBytes is the byte threshold to flush CachedRanges to the DB.
	persistBytes = 5 * 1024 * 1024 // 5 MiB
)

// Manager manages all in-progress remote→cache downloads.
type Manager struct {
	adapter remote.RemoteAdapter
	cache   *cache.CacheLayer
	monitor *connectivity.Monitor // may be nil

	mu        sync.Mutex
	downloads map[string]*Download
}

// NewManager creates a Download Manager. monitor may be nil; when non-nil,
// any active download is cancelled automatically when connectivity is lost.
func NewManager(adapter remote.RemoteAdapter, cl *cache.CacheLayer, monitor *connectivity.Monitor) *Manager {
	m := &Manager{
		adapter:   adapter,
		cache:     cl,
		monitor:   monitor,
		downloads: make(map[string]*Download),
	}
	if monitor != nil {
		go m.watchOffline()
	}
	return m
}

// Download tracks the state of a single file being downloaded.
type Download struct {
	path      string
	cacheFile *os.File
	rangeSet  *RangeSet
	totalSize int64

	mu     sync.Mutex
	cond   *sync.Cond
	seqPos int64 // current position of the sequential download goroutine
	err    error // first fatal error
	done   bool

	// goroutines maps startOffset → (cancel func, current position).
	goroutines map[int64]*goroutineInfo
	mgr        *Manager

	// Periodic persistence state.
	lastPersist      time.Time
	bytesSincePersit int64

	// finishedCh is closed (once) when the download completes or is cancelled.
	// Used to let the offline-watcher goroutine exit cleanly.
	finishedCh   chan struct{}
	finishedOnce sync.Once
}

// goroutineInfo tracks a running download goroutine.
type goroutineInfo struct {
	cancel func()
	pos    int64 // current download position (updated by the goroutine)
}

// Start begins downloading a remote file into the cache. If a download for
// the same path is already in progress, returns the existing Download (dedup).
// The returned *os.File is the cache file open for reading.
func (m *Manager) Start(path string, totalSize int64) (*Download, *os.File, error) {
	m.mu.Lock()
	if dl, ok := m.downloads[path]; ok {
		m.mu.Unlock()
		f, err := m.cache.Open(path, os.O_RDONLY)
		if err != nil {
			return nil, nil, err
		}
		return dl, f, nil
	}
	m.mu.Unlock()

	// Create (or reuse) sparse cache file.
	cacheFile, err := m.cache.OpenOrCreate(path, totalSize)
	if err != nil {
		return nil, nil, fmt.Errorf("create cache file for %q: %w", path, err)
	}

	dl := &Download{
		path:        path,
		cacheFile:   cacheFile,
		rangeSet:    NewRangeSet(),
		totalSize:   totalSize,
		goroutines:  make(map[int64]*goroutineInfo),
		mgr:         m,
		lastPersist: time.Now(),
		finishedCh:  make(chan struct{}),
	}
	dl.cond = sync.NewCond(&dl.mu)

	// Resume: load persisted CachedRanges from DB.
	if entry, err := m.cache.Stat(path); err == nil && entry != nil && entry.CachedRanges != "" {
		var rs RangeSet
		if err := json.Unmarshal([]byte(entry.CachedRanges), &rs); err == nil && rs.Len() > 0 {
			dl.rangeSet = &rs
		}
	}

	m.mu.Lock()
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

	_ = m.cache.DB().SetState(path, cache.StateDownloading)

	// Spawn sequential download goroutine from the first gap.
	seqStart := int64(0)
	if gaps := dl.rangeSet.Gaps(totalSize); len(gaps) > 0 {
		seqStart = gaps[0].Start
	}
	if !dl.rangeSet.IsComplete(totalSize) {
		dl.spawnGoroutine(seqStart, true)
	} else {
		// Already fully cached — mark done immediately.
		dl.done = true
		go dl.finish()
	}

	readFile, err := m.cache.Open(path, os.O_RDONLY)
	if err != nil {
		return nil, nil, err
	}
	return dl, readFile, nil
}

// WaitForRange blocks until [offset, offset+size) is available in the cache,
// or returns an error if the download has been cancelled or has failed.
// If the download for path is no longer tracked, it returns nil (already done).
func (m *Manager) WaitForRange(path string, offset, size int64) error {
	m.mu.Lock()
	dl, ok := m.downloads[path]
	m.mu.Unlock()

	if !ok {
		return nil
	}

	return dl.waitForRange(offset, size)
}

// Hint signals that the next read is likely at nextOffset. If the range is
// not yet downloaded and no goroutine covers it, a new one is spawned.
// Non-blocking.
func (m *Manager) Hint(path string, nextOffset int64) {
	m.mu.Lock()
	dl, ok := m.downloads[path]
	m.mu.Unlock()

	if !ok || dl == nil {
		return
	}

	dl.mu.Lock()
	defer dl.mu.Unlock()

	if dl.done || dl.err != nil {
		return
	}

	// If the hinted range is already cached, nothing to do.
	if dl.rangeSet.Contains(nextOffset, chunkSize) {
		return
	}

	// Spawn a goroutine if none is close enough.
	if !dl.hasGoroutineNear(nextOffset) {
		dl.spawnGoroutine(nextOffset, false)
	}
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

// watchOffline subscribes to the connectivity monitor and cancels all active
// downloads when the monitor transitions to StateOffline, so partial ranges
// are persisted to the DB and downloads resume cleanly on reconnect.
func (m *Manager) watchOffline() {
	ch := m.monitor.Subscribe()
	for {
		state, ok := <-ch
		if !ok {
			return
		}
		if state == connectivity.StateOffline {
			m.cancelAll()
		}
	}
}

// cancelAll cancels every active download, persisting partial CachedRanges to
// the DB as StateEvicted so they resume from where they left off on reconnect.
func (m *Manager) cancelAll() {
	m.mu.Lock()
	paths := make([]string, 0, len(m.downloads))
	for p := range m.downloads {
		paths = append(paths, p)
	}
	m.mu.Unlock()
	for _, p := range paths {
		m.Cancel(p)
	}
}

// ---------- Download methods ----------

func (dl *Download) waitForRange(offset, size int64) error {
	dl.mu.Lock()
	defer dl.mu.Unlock()

	// Fast path: already available.
	if dl.rangeSet.Contains(offset, size) {
		return nil
	}

	if dl.err != nil {
		return dl.err
	}

	// Spawn an on-demand goroutine if no goroutine is close to the
	// requested offset. This enables immediate streaming from any
	// position instead of waiting for the sequential goroutine.
	if !dl.hasGoroutineNear(offset) {
		dl.spawnGoroutine(offset, false)
	}

	// Cancel far-behind non-sequential goroutines to save bandwidth.
	dl.cancelFarBehind(offset)

	// Wait until the range is covered or an error occurs.
	for !dl.rangeSet.Contains(offset, size) && dl.err == nil {
		dl.cond.Wait()
	}

	return dl.err
}

// hasGoroutineNear returns true if any goroutine's current position is
// within goroutineWindow bytes before offset, or between offset and
// offset+goroutineWindow (i.e. downloading data that will soon cover offset).
func (dl *Download) hasGoroutineNear(offset int64) bool {
	for _, gi := range dl.goroutines {
		// Goroutine is ahead but close, or behind but close.
		if gi.pos <= offset && offset-gi.pos < goroutineWindow {
			return true
		}
		if gi.pos > offset && gi.pos-offset < goroutineWindow {
			return true
		}
	}
	return false
}

// cancelFarBehind cancels non-sequential goroutines whose position is
// more than goroutineWindow behind the requested offset.
func (dl *Download) cancelFarBehind(offset int64) {
	for startOff, gi := range dl.goroutines {
		// Never cancel the sequential goroutine (it fills the whole file).
		if startOff == dl.seqPos || gi.pos >= offset {
			continue
		}
		if offset-gi.pos > goroutineWindow {
			gi.cancel()
		}
	}
}

func (dl *Download) spawnGoroutine(startOffset int64, isSequential bool) {
	doneCh := make(chan struct{})
	gi := &goroutineInfo{
		cancel: sync.OnceFunc(func() { close(doneCh) }),
		pos:    startOffset,
	}
	dl.goroutines[startOffset] = gi

	go dl.downloadLoop(startOffset, isSequential, doneCh, gi)
}

func (dl *Download) downloadLoop(startOffset int64, isSequential bool, doneCh chan struct{}, gi *goroutineInfo) {
	defer func() {
		dl.mu.Lock()
		delete(dl.goroutines, startOffset)
		if dl.rangeSet.IsComplete(dl.totalSize) && !dl.done {
			dl.done = true
			dl.cond.Broadcast()
			go dl.finish()
		}
		dl.mu.Unlock()
	}()

	remaining := dl.totalSize - startOffset
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
	pos := startOffset

	for {
		select {
		case <-doneCh:
			pr.Close()
			return
		default:
		}

		n, err := pr.Read(buf)
		if n > 0 {
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
			pos += int64(n)
			gi.pos = pos
			if isSequential {
				dl.seqPos = pos
			}
			dl.bytesSincePersit += int64(n)
			dl.cond.Broadcast()

			// Periodically persist CachedRanges to DB for crash recovery.
			if dl.bytesSincePersit >= persistBytes || time.Since(dl.lastPersist) >= persistInterval {
				dl.persistRangesLocked(cache.StateDownloading)
			}

			// Stop if the next chunk is already downloaded (another
			// goroutine covered it) — avoids redundant network I/O.
			nextCovered := dl.rangeSet.Contains(pos, chunkSize) && pos+chunkSize <= dl.totalSize
			dl.mu.Unlock()

			if nextCovered && !isSequential {
				pr.Close()
				return
			}
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

// persistRangesLocked writes the current CachedRanges to the DB with the
// given target state. Must be called with dl.mu held.
func (dl *Download) persistRangesLocked(state cache.FileState) {
	rangesJSON, _ := dl.rangeSet.MarshalJSON()
	dl.lastPersist = time.Now()
	dl.bytesSincePersit = 0

	// Run DB update without holding the download lock.
	go func() {
		entry, err := dl.mgr.cache.Stat(dl.path)
		if err == nil && entry != nil {
			entry.CachedRanges = string(rangesJSON)
			entry.State = state
			_ = dl.mgr.cache.DB().PutFile(entry)
		}
	}()
}

func (dl *Download) closeFinished() {
	dl.finishedOnce.Do(func() { close(dl.finishedCh) })
}

func (dl *Download) finish() {
	// Signal the offline-watcher goroutine (if any) to exit before cleanup.
	dl.closeFinished()

	dl.cacheFile.Close()

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
	// Signal the offline-watcher goroutine (if any) to exit.
	dl.closeFinished()

	dl.mu.Lock()
	for _, gi := range dl.goroutines {
		gi.cancel()
	}
	if dl.err == nil {
		dl.err = fmt.Errorf("download cancelled")
	}
	dl.cond.Broadcast()

	// Persist partial ranges and transition the DB state to StateEvicted so
	// the next Open() after reconnection resumes cleanly.
	dl.persistRangesLocked(cache.StateEvicted)
	dl.mu.Unlock()

	dl.cacheFile.Close()
}

// WaitForRange blocks until [offset, offset+size) is available in the cache,
// or returns an error if the download has been cancelled. This method uses
// the Download directly rather than a path lookup in the manager, so it
// remains correct even after the download has been removed from the manager's
// active-download map (e.g. during an OFFLINE transition).
func (dl *Download) WaitForRange(offset, size int64) error {
	return dl.waitForRange(offset, size)
}
