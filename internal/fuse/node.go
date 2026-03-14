package fuse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/connectivity"
	"github.com/IstarVin/rvfs/internal/download"
	"github.com/IstarVin/rvfs/internal/remote"
	syncpkg "github.com/IstarVin/rvfs/internal/sync"
	"github.com/hanwen/go-fuse/v2/fs"
	gofuse "github.com/hanwen/go-fuse/v2/fuse"
)

const (
	statfsBlockSize   = 4096
	defaultStatfsFile = 1 << 20
)

// RootState holds shared state for all nodes in the FUSE tree.
type RootState struct {
	cache       *cache.CacheLayer
	adapter     remote.RemoteAdapter
	downloadMgr *download.Manager     // nil when using backing-dir mode
	monitor     *connectivity.Monitor // nil when using backing-dir mode
	syncEngine  *syncpkg.Engine       // nil when using backing-dir mode

	// writeMu serialises concurrent writes to the same path within this
	// process. The map stores *sync.Mutex values keyed by relative path.
	writeMu sync.Map

	// verifyChecksums, when true, hashes clean cache files on Open and
	// evicts them if the checksum does not match the stored value.
	verifyChecksums bool

	quotaTTL       time.Duration
	quotaMu        sync.Mutex
	quota          remote.QuotaInfo
	quotaFetchedAt time.Time
	quotaValid     bool
}

// FuseNode is a node in the FUSE filesystem tree.
// Each node corresponds to one path relative to the cache root.
type FuseNode struct {
	fs.Inode
	rel  string // path relative to cache root, "" for root
	root *RootState
}

// inodeFor computes a stable inode number from a relative path using FNV-64a.
// Using the backing sys.Ino would cause stale paths after renames; using 0 causes
// go-fuse to auto-assign IDs that change after forget+recreate ("replaced while
// being copied" errors). FNV hash of the path is stable and unique per path.
func inodeFor(rel string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(rel))
	v := h.Sum64()
	// Ensure non-zero (zero triggers go-fuse auto-assign).
	if v == 0 {
		return 1
	}
	return v
}

// lockForWrite returns the per-path write mutex for rel, creating it if
// necessary, and returns it locked. The caller must call the returned unlock
// function when the write is complete.
func (r *RootState) lockForWrite(rel string) (unlock func()) {
	v, _ := r.writeMu.LoadOrStore(rel, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// childRel returns the relative path for a child of this node.
func (n *FuseNode) childRel(name string) string {
	if n.rel == "" {
		return name
	}
	return n.rel + "/" + name
}

// fillAttr populates a fuse.Attr from a syscall.Stat_t.
// Always use Stat_t.Mode directly — it is POSIX-encoded, unlike os.FileMode.
func fillAttr(st *syscall.Stat_t, out *gofuse.Attr) {
	out.Ino = st.Ino
	out.Size = uint64(st.Size)
	out.Blocks = uint64(st.Blocks)
	out.Atime = uint64(st.Atim.Sec)
	out.Atimensec = uint32(st.Atim.Nsec)
	out.Mtime = uint64(st.Mtim.Sec)
	out.Mtimensec = uint32(st.Mtim.Nsec)
	out.Ctime = uint64(st.Ctim.Sec)
	out.Ctimensec = uint32(st.Ctim.Nsec)
	out.Mode = st.Mode // POSIX mode bits including file type
	out.Nlink = uint32(st.Nlink)
	out.Uid = st.Uid
	out.Gid = st.Gid
	out.Rdev = uint32(st.Rdev)
	out.Blksize = uint32(st.Blksize)
}

// fillAttrFromEntry populates a fuse.Attr from a cache.FileEntry.
// Fields not tracked in the DB (uid, gid, timestamps) use reasonable defaults.
func fillAttrFromEntry(e *cache.FileEntry, out *gofuse.Attr) {
	out.Mode = e.Mode
	out.Size = uint64(e.Size)
	if e.IsDir {
		out.Nlink = 2
	} else {
		out.Nlink = 1
	}
	out.Mtime = uint64(e.LocalMtime)
	out.Atime = uint64(e.LocalMtime)
	out.Ctime = uint64(e.LocalMtime)
	out.Uid = uint32(os.Getuid())
	out.Gid = uint32(os.Getgid())
	out.Blksize = 4096
	out.Blocks = (uint64(e.Size) + 511) / 512
}

// toErrno converts an error to a syscall.Errno.
func toErrno(err error) syscall.Errno {
	if errno, ok := err.(syscall.Errno); ok {
		return errno
	}
	if pe, ok := err.(*os.PathError); ok {
		if errno, ok := pe.Err.(syscall.Errno); ok {
			return errno
		}
	}
	return syscall.EIO
}

func (r *RootState) quotaSnapshot(ctx context.Context) (remote.QuotaInfo, bool) {
	if r.adapter == nil {
		return remote.QuotaInfo{}, false
	}

	r.quotaMu.Lock()
	defer r.quotaMu.Unlock()

	now := time.Now()
	if r.quotaValid && (r.quotaTTL <= 0 || now.Sub(r.quotaFetchedAt) < r.quotaTTL) {
		return r.quota, true
	}

	quota, err := r.adapter.Quota(ctx)
	if err == nil {
		quota = quota.Normalized()
		if quota.Valid() {
			r.quota = quota
			r.quotaFetchedAt = now
			r.quotaValid = true
			return quota, true
		}
	}

	if r.quotaValid {
		return r.quota, true
	}

	return remote.QuotaInfo{}, false
}

func fillStatfsFromSystem(st *syscall.Statfs_t, out *gofuse.StatfsOut) {
	out.Blocks = st.Blocks
	out.Bfree = st.Bfree
	out.Bavail = st.Bavail
	out.Files = st.Files
	out.Ffree = st.Ffree
	out.Bsize = uint32(st.Bsize)
	out.Frsize = uint32(st.Frsize)
	out.NameLen = uint32(st.Namelen)
}

func fillStatfsFromQuota(quota remote.QuotaInfo, out *gofuse.StatfsOut) {
	quota = quota.Normalized()
	if !quota.Valid() {
		return
	}

	blocks := uint64((quota.TotalBytes + statfsBlockSize - 1) / statfsBlockSize)
	available := uint64(quota.AvailableBytes / statfsBlockSize)
	used := uint64(quota.UsedBytes / statfsBlockSize)
	free := available
	if blocks > used && blocks-used < free {
		free = blocks - used
	}

	out.Blocks = blocks
	out.Bfree = free
	out.Bavail = available
	out.Files = defaultStatfsFile
	out.Ffree = defaultStatfsFile
	out.Bsize = statfsBlockSize
	out.Frsize = statfsBlockSize
	out.NameLen = 255
}

// --- NodeLookuper ---

var _ fs.NodeLookuper = (*FuseNode)(nil)

func (n *FuseNode) Lookup(ctx context.Context, name string, out *gofuse.EntryOut) (*fs.Inode, syscall.Errno) {
	rel := n.childRel(name)

	entry, err := n.root.cache.Stat(rel)
	if err != nil {
		return nil, syscall.EIO
	}
	if entry == nil {
		return nil, syscall.ENOENT
	}

	fillAttrFromEntry(entry, &out.Attr)

	// For evicted entries the file doesn't exist on disk; skip LstatDisk.
	if entry.State != cache.StateEvicted {
		if st, err := n.root.cache.LstatDisk(rel); err == nil {
			fillAttr(st, &out.Attr)
		}
	}

	child := n.NewInode(ctx, &FuseNode{rel: rel, root: n.root}, fs.StableAttr{
		Mode: entry.Mode,
		Ino:  inodeFor(rel),
	})
	return child, 0
}

// --- NodeStatfser ---

var _ fs.NodeStatfser = (*FuseNode)(nil)

func (n *FuseNode) Statfs(ctx context.Context, out *gofuse.StatfsOut) syscall.Errno {
	if quota, ok := n.root.quotaSnapshot(ctx); ok {
		fillStatfsFromQuota(quota, out)
		if out.Blocks > 0 {
			return 0
		}
	}

	var st syscall.Statfs_t
	if err := syscall.Statfs(n.root.cache.FilesDir(), &st); err != nil {
		return toErrno(err)
	}
	fillStatfsFromSystem(&st, out)
	return 0
}

// --- NodeGetattrer ---

var _ fs.NodeGetattrer = (*FuseNode)(nil)

func (n *FuseNode) Getattr(ctx context.Context, fh fs.FileHandle, out *gofuse.AttrOut) syscall.Errno {
	if n.rel == "" {
		// Root node: stat the cache files directory.
		st, err := n.root.cache.LstatDisk("")
		if err != nil {
			return toErrno(err)
		}
		fillAttr(st, &out.Attr)
		return 0
	}

	entry, err := n.root.cache.Stat(n.rel)
	if err != nil {
		return syscall.EIO
	}
	if entry == nil {
		return syscall.ENOENT
	}

	fillAttrFromEntry(entry, &out.Attr)

	// For evicted entries the file doesn't exist on disk; skip LstatDisk.
	if entry.State != cache.StateEvicted {
		if st, err := n.root.cache.LstatDisk(n.rel); err == nil {
			fillAttr(st, &out.Attr)
		}
	}
	return 0
}

// --- NodeSetattrer ---

var _ fs.NodeSetattrer = (*FuseNode)(nil)

func (n *FuseNode) Setattr(ctx context.Context, fh fs.FileHandle, in *gofuse.SetAttrIn, out *gofuse.AttrOut) syscall.Errno {
	rel := n.rel

	if mode, ok := in.GetMode(); ok {
		if err := n.root.cache.Chmod(rel, os.FileMode(mode)); err != nil {
			return toErrno(err)
		}
	}

	if sz, ok := in.GetSize(); ok {
		if err := n.root.cache.Truncate(rel, int64(sz)); err != nil {
			return toErrno(err)
		}
	}

	// SetAttrIn uses Atimensec/Mtimensec (NOT AtimeNsec/MtimeNsec).
	atime, aok := in.GetATime()
	mtime, mok := in.GetMTime()
	if aok || mok {
		if !aok || !mok {
			st, err := n.root.cache.LstatDisk(rel)
			if err != nil {
				return toErrno(err)
			}
			if !aok {
				atime = time.Unix(st.Atim.Sec, st.Atim.Nsec)
			}
			if !mok {
				mtime = time.Unix(st.Mtim.Sec, st.Mtim.Nsec)
			}
		}
		if err := n.root.cache.Chtimes(rel, atime, mtime); err != nil {
			return toErrno(err)
		}
	}

	st, err := n.root.cache.LstatDisk(rel)
	if err != nil {
		return toErrno(err)
	}
	fillAttr(st, &out.Attr)
	return 0
}

// --- NodeReaddirer ---

var _ fs.NodeReaddirer = (*FuseNode)(nil)

func (n *FuseNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	entries, err := n.root.cache.ReadDir(n.rel)
	if err != nil {
		return nil, syscall.EIO
	}

	var list []gofuse.DirEntry
	for _, e := range entries {
		list = append(list, gofuse.DirEntry{
			Mode: e.Mode,
			Name: filepath.Base(e.Path),
			Ino:  inodeFor(e.Path),
		})
	}
	return fs.NewListDirStream(list), 0
}

// --- NodeOpener ---

var _ fs.NodeOpener = (*FuseNode)(nil)

func (n *FuseNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	// Check if this file needs to be downloaded from remote.
	if n.root.downloadMgr != nil {
		entry, err := n.root.cache.Stat(n.rel)
		if err != nil {
			return nil, 0, syscall.EIO
		}
		if entry != nil && (entry.State == cache.StateEvicted || entry.State == cache.StateDownloading) {
			return &downloadFileHandle{
				path:    n.rel,
				totalSz: entry.Size,
				mgr:     n.root.downloadMgr,
				monitor: n.root.monitor,
			}, 0, 0
		}
	}

	f, err := n.root.cache.Open(n.rel, int(flags))
	if err != nil {
		return nil, 0, toErrno(err)
	}

	// Optional checksum verification: if enabled and a stored checksum exists,
	// hash the cache file and evict it on mismatch to force re-download.
	if n.root.verifyChecksums {
		entry, _ := n.root.cache.Stat(n.rel)
		if entry != nil && entry.Checksum != "" {
			if corrupt := verifyCacheFile(f, entry.Checksum); corrupt {
				f.Close()
				_ = n.root.cache.DB().MarkEvicted(n.rel)
				// Retry Open — subsequent reads will trigger a fresh download.
				return n.Open(ctx, flags)
			}
		}
	}

	return &fileHandle{f: f, rel: n.rel, root: n.root}, 0, 0
}

// --- NodeCreater ---

var _ fs.NodeCreater = (*FuseNode)(nil)

func (n *FuseNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *gofuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	rel := n.childRel(name)

	f, entry, err := n.root.cache.Create(rel, mode)
	if err != nil {
		return nil, nil, 0, toErrno(err)
	}

	fillAttrFromEntry(entry, &out.Attr)

	// Get full POSIX attrs from the open fd.
	if st, err := n.root.cache.FstatDisk(int(f.Fd())); err == nil {
		fillAttr(st, &out.Attr)
	}

	child := n.NewInode(ctx, &FuseNode{rel: rel, root: n.root}, fs.StableAttr{
		Mode: entry.Mode,
		Ino:  inodeFor(rel),
	})
	return child, &fileHandle{f: f, rel: rel, root: n.root}, 0, 0
}

// --- NodeUnlinker ---

var _ fs.NodeUnlinker = (*FuseNode)(nil)

func (n *FuseNode) Unlink(ctx context.Context, name string) syscall.Errno {
	rel := n.childRel(name)

	// Cancel any in-progress download for this path before removing the file.
	if n.root.downloadMgr != nil {
		n.root.downloadMgr.Cancel(rel)
	}
	// Cancel any in-flight upload for this path.
	if n.root.syncEngine != nil {
		n.root.syncEngine.CancelUpload(rel)
	}

	if err := n.root.cache.Delete(rel); err != nil {
		return toErrno(err)
	}
	return 0
}

// --- NodeMkdirer ---

var _ fs.NodeMkdirer = (*FuseNode)(nil)

func (n *FuseNode) Mkdir(ctx context.Context, name string, mode uint32, out *gofuse.EntryOut) (*fs.Inode, syscall.Errno) {
	rel := n.childRel(name)

	entry, err := n.root.cache.Mkdir(rel, mode)
	if err != nil {
		return nil, toErrno(err)
	}

	fillAttrFromEntry(entry, &out.Attr)

	if st, err := n.root.cache.LstatDisk(rel); err == nil {
		fillAttr(st, &out.Attr)
	}
	// out.Attr.Mode must include S_IFDIR (from Stat_t.Mode or entry.Mode).
	// The go-fuse bridge asserts: out.Attr.Mode &^ 07777 == syscall.S_IFDIR.

	child := n.NewInode(ctx, &FuseNode{rel: rel, root: n.root}, fs.StableAttr{
		Mode: entry.Mode,
		Ino:  inodeFor(rel),
	})
	return child, 0
}

// --- NodeRmdirer ---

var _ fs.NodeRmdirer = (*FuseNode)(nil)

func (n *FuseNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	rel := n.childRel(name)
	if err := n.root.cache.Rmdir(rel); err != nil {
		return toErrno(err)
	}
	return 0
}

// --- NodeRenamer ---

var _ fs.NodeRenamer = (*FuseNode)(nil)

func (n *FuseNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	newParentNode, ok := newParent.(*FuseNode)
	if !ok {
		return syscall.EINVAL
	}
	oldRel := n.childRel(name)
	newRel := newParentNode.childRel(newName)
	if err := n.root.cache.Rename(oldRel, newRel); err != nil {
		return toErrno(err)
	}
	return 0
}

// --- fileHandle ---

// fileHandle wraps an *os.File and implements fs.FileHandle with Read/Write.
type fileHandle struct {
	f    *os.File
	rel  string
	root *RootState
}

var _ fs.FileReader = (*fileHandle)(nil)
var _ fs.FileWriter = (*fileHandle)(nil)
var _ fs.FileReleaser = (*fileHandle)(nil)

func (fh *fileHandle) Read(ctx context.Context, dest []byte, off int64) (gofuse.ReadResult, syscall.Errno) {
	n, err := fh.f.ReadAt(dest, off)
	if err != nil && err != io.EOF {
		return nil, syscall.EIO
	}
	return gofuse.ReadResultData(dest[:n]), 0
}

func (fh *fileHandle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	unlock := fh.root.lockForWrite(fh.rel)
	defer unlock()
	n, err := fh.f.WriteAt(data, off)
	if err != nil {
		return uint32(n), toErrno(err)
	}
	return uint32(n), 0
}

func (fh *fileHandle) Release(ctx context.Context) syscall.Errno {
	if err := fh.f.Close(); err != nil {
		return toErrno(err)
	}
	return 0
}

// --- downloadFileHandle ---

// downloadFileHandle wraps a file being downloaded from a remote. Read calls
// block until the requested range is available. Write is not supported.
type downloadFileHandle struct {
	mu      sync.Mutex
	f       *os.File
	dl      *download.Download
	path    string
	totalSz int64
	mgr     *download.Manager
	monitor *connectivity.Monitor
}

var _ fs.FileReader = (*downloadFileHandle)(nil)
var _ fs.FileReleaser = (*downloadFileHandle)(nil)

func (dh *downloadFileHandle) ensureStarted() syscall.Errno {
	dh.mu.Lock()
	defer dh.mu.Unlock()

	if dh.dl != nil {
		return 0
	}

	// Opening an evicted file should succeed even while offline, but the first
	// read must fail until connectivity is restored.
	if dh.monitor != nil && dh.monitor.State() == connectivity.StateOffline {
		return syscall.ENOENT
	}

	dl, readFile, err := dh.mgr.Start(dh.path, dh.totalSz)
	if err != nil {
		return toErrno(err)
	}

	dh.dl = dl
	dh.f = readFile
	return 0
}

func (dh *downloadFileHandle) Read(ctx context.Context, dest []byte, off int64) (gofuse.ReadResult, syscall.Errno) {
	if errno := dh.ensureStarted(); errno != 0 {
		return nil, errno
	}

	// Use the Download reference directly so range checks remain correct
	// even after the download has been removed from the manager's map
	// (e.g. following an OFFLINE cancellation).
	if err := dh.dl.WaitForRange(off, int64(len(dest))); err != nil {
		return nil, syscall.EIO
	}
	n, err := dh.f.ReadAt(dest, off)
	if err != nil && err != io.EOF {
		return nil, syscall.EIO
	}

	// Hint the download manager about the next likely read position
	// so it can prefetch data ahead of the reader for smooth streaming.
	dh.mgr.Hint(dh.path, off+int64(n))

	return gofuse.ReadResultData(dest[:n]), 0
}

func (dh *downloadFileHandle) Release(ctx context.Context) syscall.Errno {
	dh.mu.Lock()
	f := dh.f
	dl := dh.dl
	dh.f = nil
	dh.dl = nil
	dh.mu.Unlock()

	var err error
	if f != nil {
		err = f.Close()
	}
	if dl != nil {
		dl.ReleaseReader()
	}
	if err != nil {
		return toErrno(err)
	}
	return 0
}

// verifyCacheFile computes the SHA256 of f (rewinding afterwards) and returns
// true if the hash does not match expectedHex, indicating corruption.
func verifyCacheFile(f *os.File, expectedHex string) (corrupt bool) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false // cannot seek — skip verification
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	got := hex.EncodeToString(h.Sum(nil))
	// Rewind so the caller can still read the file normally.
	_, _ = f.Seek(0, io.SeekStart)
	return got != expectedHex
}
