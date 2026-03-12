package fuse

import (
	"context"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/connectivity"
	"github.com/IstarVin/rvfs/internal/download"
	"github.com/hanwen/go-fuse/v2/fs"
	gofuse "github.com/hanwen/go-fuse/v2/fuse"
)

// RootState holds shared state for all nodes in the FUSE tree.
type RootState struct {
	cache       *cache.CacheLayer
	downloadMgr *download.Manager     // nil when using backing-dir mode
	monitor     *connectivity.Monitor // nil when using backing-dir mode
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
			// Refuse to start a new download while offline — the file is not
			// locally available and we cannot reach the remote.
			if entry.State == cache.StateEvicted &&
				n.root.monitor != nil &&
				n.root.monitor.State() == connectivity.StateOffline {
				return nil, 0, syscall.ENOENT
			}
			dl, readFile, err := n.root.downloadMgr.Start(n.rel, entry.Size)
			if err != nil {
				return nil, 0, syscall.EIO
			}
			return &downloadFileHandle{
				f:    readFile,
				dl:   dl,
				path: n.rel,
				mgr:  n.root.downloadMgr,
			}, 0, 0
		}
	}

	f, err := n.root.cache.Open(n.rel, int(flags))
	if err != nil {
		return nil, 0, toErrno(err)
	}
	return &fileHandle{f: f}, 0, 0
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
	return child, &fileHandle{f: f}, 0, 0
}

// --- NodeUnlinker ---

var _ fs.NodeUnlinker = (*FuseNode)(nil)

func (n *FuseNode) Unlink(ctx context.Context, name string) syscall.Errno {
	rel := n.childRel(name)
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
	f *os.File
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
	f    *os.File
	dl   *download.Download
	path string
	mgr  *download.Manager
}

var _ fs.FileReader = (*downloadFileHandle)(nil)
var _ fs.FileReleaser = (*downloadFileHandle)(nil)

func (dh *downloadFileHandle) Read(ctx context.Context, dest []byte, off int64) (gofuse.ReadResult, syscall.Errno) {
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
	if err := dh.f.Close(); err != nil {
		return toErrno(err)
	}
	return 0
}
