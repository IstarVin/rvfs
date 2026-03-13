package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"sync/atomic"
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

// ManagerOptions configures optional behaviour of the download Manager.
type ManagerOptions struct {
	// ReadAhead is the maximum number of bytes the sequential download goroutine
	// is allowed to get ahead of the furthest position the reader has consumed.
	// 0 means unlimited — the goroutine downloads as fast as possible (default
	// behaviour).
	ReadAhead int64

	// IdleTimeout stops the sequential download goroutine when it has been
	// paused at the read-ahead limit for this long with no new reads. The
	// goroutine is automatically restarted from the reader's position on the
	// next read. 0 means wait indefinitely.
	// Only meaningful when ReadAhead > 0.
	IdleTimeout time.Duration
}

// Manager manages all in-progress remote→cache downloads.
type Manager struct {
	adapter remote.RemoteAdapter
	cache   *cache.CacheLayer
	monitor *connectivity.Monitor // may be nil

	readAhead   int64         // copy of ManagerOptions.ReadAhead
	idleTimeout time.Duration // copy of ManagerOptions.IdleTimeout

	mu        sync.Mutex
	downloads map[string]*Download
}

// NewManager creates a Download Manager. monitor may be nil; when non-nil,
// any active download is cancelled automatically when connectivity is lost.
func NewManager(adapter remote.RemoteAdapter, cl *cache.CacheLayer, monitor *connectivity.Monitor, opts ManagerOptions) *Manager {
	m := &Manager{
		adapter:     adapter,
		cache:       cl,
		monitor:     monitor,
		readAhead:   opts.ReadAhead,
		idleTimeout: opts.IdleTimeout,
		downloads:   make(map[string]*Download),
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

	// openCount is the number of active downloadFileHandle readers.
	// When it drops to zero and readAhead > 0, the download is cancelled so
	// bandwidth is not wasted after the last reader closes the file.
	// Manipulated with atomic operations; does not require dl.mu.
	openCount atomic.Int64

	mu          sync.Mutex
	cond        *sync.Cond
	prefetch    bool  // true when download was explicitly requested via Prefetch
	seqPos      int64 // current position of the sequential download goroutine
	lastReadPos int64 // furthest offset+size the reader has consumed
	err         error // first fatal error
	done        bool

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
	cancel       func()
	pos          int64 // current download position (updated by the goroutine)
	isSequential bool  // true for the single sequential fill goroutine
}

// Progress is a snapshot of one path's active download state.
type Progress struct {
	Path       string
	State      string
	Downloaded int64
	TotalSize  int64
	Done       bool
	Err        string
}

// Start begins downloading a remote file into the cache. If a download for
// the same path is already in progress, returns the existing Download (dedup).
// The returned *os.File is the cache file open for reading.
func (m *Manager) Start(path string, totalSize int64) (*Download, *os.File, error) {
	m.mu.Lock()
	if dl, ok := m.downloads[path]; ok {
		dl.openCount.Add(1)
		m.mu.Unlock()
		f, err := m.cache.Open(path, os.O_RDONLY)
		if err != nil {
			dl.openCount.Add(-1)
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
	dl.openCount.Store(1)

	// Resume: load persisted CachedRanges from DB.
	if entry, err := m.cache.Stat(path); err == nil && entry != nil && entry.CachedRanges != "" {
		var rs RangeSet
		if err := json.Unmarshal([]byte(entry.CachedRanges), &rs); err == nil && rs.Len() > 0 {
			dl.rangeSet = &rs
		}
	}

	m.mu.Lock()
	if existing, ok := m.downloads[path]; ok {
		existing.openCount.Add(1)
		m.mu.Unlock()
		cacheFile.Close()
		f, err := m.cache.Open(path, os.O_RDONLY)
		if err != nil {
			existing.openCount.Add(-1)
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

// Prefetch starts (or resumes) an asynchronous download for path.
// It does not require a live reader handle.
func (m *Manager) Prefetch(path string, totalSize int64) error {
	m.mu.Lock()
	if dl, ok := m.downloads[path]; ok {
		dl.mu.Lock()
		dl.prefetch = true
		dl.cond.Broadcast()
		dl.mu.Unlock()
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	dl, f, err := m.Start(path, totalSize)
	if err != nil {
		return err
	}
	_ = f.Close()

	dl.mu.Lock()
	dl.prefetch = true
	dl.cond.Broadcast()
	dl.mu.Unlock()

	// Balance the reader count added by Start() without cancelling the
	// download: prefetch=true keeps ReleaseReader from stopping it.
	dl.ReleaseReader()
	return nil
}

// Snapshots returns active download progress. If path is non-empty, at most
// one entry (for that path) is returned.
func (m *Manager) Snapshots(path string) []Progress {
	m.mu.Lock()
	list := make([]*Download, 0, len(m.downloads))
	if path != "" {
		if dl, ok := m.downloads[path]; ok {
			list = append(list, dl)
		}
	} else {
		for _, dl := range m.downloads {
			list = append(list, dl)
		}
	}
	m.mu.Unlock()

	out := make([]Progress, 0, len(list))
	for _, dl := range list {
		dl.mu.Lock()
		p := Progress{
			Path:       dl.path,
			Downloaded: coveredBytes(dl.rangeSet.Intervals(), dl.totalSize),
			TotalSize:  dl.totalSize,
			Done:       dl.done,
		}
		switch {
		case dl.done:
			p.State = "complete"
		case dl.err != nil:
			p.State = "error"
			p.Err = dl.err.Error()
		default:
			p.State = "downloading"
		}
		dl.mu.Unlock()
		out = append(out, p)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func coveredBytes(iv []Interval, totalSize int64) int64 {
	if totalSize <= 0 {
		return 0
	}
	var n int64
	for _, r := range iv {
		start := max(r.Start, 0)
		end := min(r.End, totalSize)
		if end > start {
			n += end - start
		}
	}
	if n > totalSize {
		return totalSize
	}
	return n
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
	// Clamp size to the file boundary so we never wait for bytes past EOF.
	// FUSE can issue aligned reads (e.g. 128 KiB) that cross the end of file;
	// Contains(offset, size) would return false forever in that case even after
	// the download completes, causing a permanent hang.
	if offset >= dl.totalSize {
		return nil
	}
	if offset+size > dl.totalSize {
		size = dl.totalSize - offset
	}

	dl.mu.Lock()
	defer dl.mu.Unlock()

	// Track the furthest read position so the sequential download goroutine
	// can stay within the configured read-ahead limit. Do this before the fast
	// path so even cache-hit reads keep the goroutine moving forward.
	if end := offset + size; end > dl.lastReadPos {
		dl.lastReadPos = end
	}
	// Broadcast to wake any goroutine currently paused at the read-ahead limit.
	dl.cond.Broadcast()

	// Fast path: already available.
	if dl.rangeSet.Contains(offset, size) {
		return nil
	}

	if dl.err != nil {
		return dl.err
	}

	// If the download is already marked done the whole file is present;
	// Contains would have returned true above for any in-bounds range.
	if dl.done {
		return nil
	}

	// If all goroutines have exited (e.g. the sequential goroutine stopped due
	// to the idle timeout), restart a sequential goroutine from this offset so
	// prefetching resumes automatically when the reader continues.
	if len(dl.goroutines) == 0 {
		dl.spawnGoroutine(offset, true)
	} else if !dl.hasGoroutineNear(offset) {
		if dl.openCount.Load() == 1 {
			// Single reader seeking to a distant position — redirect all
			// goroutines so bandwidth is focused on what is being read.
			for _, gi := range dl.goroutines {
				gi.cancel()
			}
			dl.spawnGoroutine(offset, true)
		} else {
			// Multiple readers at different positions — keep existing
			// goroutines running and add a secondary one for this reader.
			dl.spawnGoroutine(offset, false)
		}
	}

	// Cancel far-behind non-sequential goroutines to save bandwidth.
	dl.cancelFarBehind(offset)

	// Wait until the range is covered, an error occurs, or the download finishes.
	for !dl.rangeSet.Contains(offset, size) && dl.err == nil && !dl.done {
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
	for _, gi := range dl.goroutines {
		// Never cancel the sequential goroutine (it fills the whole file).
		// Using gi.isSequential is correct; seqPos is the *current* position
		// of the goroutine and cannot be used to identify it by start offset.
		if gi.isSequential || gi.pos >= offset {
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
		cancel:       sync.OnceFunc(func() { close(doneCh) }),
		pos:          startOffset,
		isSequential: isSequential,
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
		} else if len(dl.goroutines) == 0 && dl.err != nil && !dl.done {
			// All goroutines have exited due to error — remove from manager map
			// so future Start() calls can retry the download.
			dl.done = true
			dl.cond.Broadcast()
			go func() {
				dl.mgr.mu.Lock()
				delete(dl.mgr.downloads, dl.path)
				dl.mgr.mu.Unlock()
			}()
		}
		dl.mu.Unlock()
	}()

	remaining := dl.totalSize - startOffset
	if remaining <= 0 {
		return
	}

	pr, pw := io.Pipe()

	// gCtx is cancelled when doneCh closes (manual cancel) or when the
	// download goroutine exits naturally, so the adapter call cleans up.
	gCtx, gCancel := context.WithCancel(context.Background())
	go func() {
		defer gCancel()
		var err error
		if isSequential && startOffset == 0 {
			err = dl.mgr.adapter.Get(gCtx, dl.path, pw)
		} else {
			length := dl.totalSize - startOffset
			err = dl.mgr.adapter.GetRange(gCtx, dl.path, startOffset, length, pw)
		}
		pw.CloseWithError(err)
	}()
	// Cancel gCtx if the goroutine is stopped via doneCh before the
	// download goroutine exits on its own.
	go func() {
		select {
		case <-doneCh:
			gCancel()
		case <-gCtx.Done():
		}
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

			// Read-ahead throttle (sequential goroutine only).
			// When the sequential goroutine is more than readAhead bytes
			// ahead of the furthest position the reader has consumed, pause
			// here until the reader catches up. This stops network I/O while
			// the user has paused playback.
			if isSequential && dl.mgr.readAhead > 0 {
				dl.mu.Lock()
				pausedSince := time.Now()
				for !dl.prefetch && pos > dl.lastReadPos+dl.mgr.readAhead && !dl.done && dl.err == nil {
					// If an idle timeout is configured and has elapsed while
					// we've been waiting, stop downloading entirely. The
					// goroutine will be restarted automatically by the next
					// waitForRange call (e.g. the player resuming).
					if dl.mgr.idleTimeout > 0 && time.Since(pausedSince) >= dl.mgr.idleTimeout {
						dl.mu.Unlock()
						pr.Close()
						return
					}
					if dl.mgr.idleTimeout > 0 {
						// Schedule a short wake-up so we can re-evaluate the
						// idle timeout even when no read Broadcasts occur.
						timer := time.AfterFunc(100*time.Millisecond, func() {
							dl.mu.Lock()
							dl.cond.Broadcast()
							dl.mu.Unlock()
						})
						dl.cond.Wait()
						timer.Stop()
					} else {
						// No idle timeout: block until a read unblocks us.
						dl.cond.Wait()
					}
				}
				shouldStop := dl.done || dl.err != nil
				dl.mu.Unlock()
				if shouldStop {
					pr.Close()
					return
				}
			}
		}

		if err != nil {
			if err != io.EOF {
				// Non-sequential goroutines are cancelled intentionally (e.g. when
				// they fall far behind the active read position). Do not promote
				// that cancellation into a fatal download error.
				select {
				case <-doneCh:
					pr.Close()
					return
				default:
				}
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
			// Background range checkpoints must never downgrade a terminal state
			// (e.g. Evicted/Clean) back to Downloading due to async races.
			if state != cache.StateDownloading || entry.State == cache.StateDownloading {
				entry.State = state
			}
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

	// Compute SHA256 checksum of the completed cache file before closing it.
	checksum := ""
	if _, err := dl.cacheFile.Seek(0, io.SeekStart); err == nil {
		h := sha256.New()
		if _, err := io.Copy(h, dl.cacheFile); err == nil {
			checksum = hex.EncodeToString(h.Sum(nil))
		}
	}

	dl.cacheFile.Close()

	rangesJSON, _ := dl.rangeSet.MarshalJSON()
	entry, err := dl.mgr.cache.Stat(dl.path)
	if err == nil && entry != nil {
		entry.State = cache.StateClean
		entry.CachedRanges = string(rangesJSON)
		if checksum != "" {
			entry.Checksum = checksum
		}
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
	rangesJSON, _ := dl.rangeSet.MarshalJSON()
	dl.mu.Unlock()

	// Persist partial ranges synchronously and transition the DB state to
	// StateEvicted before returning so process exit cannot lose this write.
	entry, err := dl.mgr.cache.Stat(dl.path)
	if err == nil && entry != nil {
		entry.CachedRanges = string(rangesJSON)
		entry.State = cache.StateEvicted
		_ = dl.mgr.cache.DB().PutFile(entry)
	}

	dl.cacheFile.Close()
}

// ReleaseReader decrements the open-handle count. When the last reader closes
// and ReadAhead > 0, the download is cancelled so we don't burn bandwidth
// after the player exits. Persisted ranges allow the next Open to resume.
func (dl *Download) ReleaseReader() {
	if dl.openCount.Add(-1) > 0 || dl.mgr.readAhead == 0 {
		return
	}
	dl.mu.Lock()
	done := dl.done
	prefetch := dl.prefetch
	dl.mu.Unlock()
	if !done && !prefetch {
		dl.mgr.Cancel(dl.path)
	}
}

// WaitForRange blocks until [offset, offset+size) is available in the cache,
// or returns an error if the download has been cancelled. This method uses
// the Download directly rather than a path lookup in the manager, so it
// remains correct even after the download has been removed from the manager's
// active-download map (e.g. during an OFFLINE transition).
func (dl *Download) WaitForRange(offset, size int64) error {
	return dl.waitForRange(offset, size)
}
