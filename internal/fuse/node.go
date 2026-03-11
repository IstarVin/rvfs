package fuse

import (
	"context"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	gofuse "github.com/hanwen/go-fuse/v2/fuse"
)

// RootState holds shared state for all nodes in the FUSE tree.
type RootState struct {
	backingDir string
}

// FuseNode is a node in the FUSE filesystem tree.
// Each node corresponds to one path relative to the backing directory.
type FuseNode struct {
	fs.Inode
	rel   string // path relative to backingDir, "" for root
	root  *RootState
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

// pathOf returns the absolute backing-dir path for this node.
func (n *FuseNode) pathOf() string {
	if n.rel == "" {
		return n.root.backingDir
	}
	return filepath.Join(n.root.backingDir, n.rel)
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

// --- NodeLookuper ---

var _ fs.NodeLookuper = (*FuseNode)(nil)

func (n *FuseNode) Lookup(ctx context.Context, name string, out *gofuse.EntryOut) (*fs.Inode, syscall.Errno) {
	rel := n.childRel(name)
	fullPath := filepath.Join(n.root.backingDir, rel)

	var st syscall.Stat_t
	if err := syscall.Lstat(fullPath, &st); err != nil {
		return nil, err.(syscall.Errno)
	}

	fillAttr(&st, &out.Attr)

	child := n.NewInode(ctx, &FuseNode{rel: rel, root: n.root}, fs.StableAttr{
		Mode: st.Mode,
		Ino:  inodeFor(rel),
	})
	return child, 0
}

// --- NodeGetattrer ---

var _ fs.NodeGetattrer = (*FuseNode)(nil)

func (n *FuseNode) Getattr(ctx context.Context, fh fs.FileHandle, out *gofuse.AttrOut) syscall.Errno {
	var st syscall.Stat_t
	if err := syscall.Lstat(n.pathOf(), &st); err != nil {
		return err.(syscall.Errno)
	}
	fillAttr(&st, &out.Attr)
	return 0
}

// --- NodeSetattrer ---

var _ fs.NodeSetattrer = (*FuseNode)(nil)

func (n *FuseNode) Setattr(ctx context.Context, fh fs.FileHandle, in *gofuse.SetAttrIn, out *gofuse.AttrOut) syscall.Errno {
	p := n.pathOf()

	if mode, ok := in.GetMode(); ok {
		if err := os.Chmod(p, os.FileMode(mode)); err != nil {
			return err.(*os.PathError).Err.(syscall.Errno)
		}
	}

	if sz, ok := in.GetSize(); ok {
		if err := os.Truncate(p, int64(sz)); err != nil {
			return err.(*os.PathError).Err.(syscall.Errno)
		}
	}

	// SetAttrIn uses Atimensec/Mtimensec (NOT AtimeNsec/MtimeNsec).
	atime, aok := in.GetATime()
	mtime, mok := in.GetMTime()
	if aok || mok {
		if !aok || !mok {
			// Fill in whichever side wasn't set from the current stat.
			var st syscall.Stat_t
			if err := syscall.Lstat(p, &st); err != nil {
				return err.(syscall.Errno)
			}
			if !aok {
				atime = time.Unix(st.Atim.Sec, st.Atim.Nsec)
			}
			if !mok {
				mtime = time.Unix(st.Mtim.Sec, st.Mtim.Nsec)
			}
		}
		if err := os.Chtimes(p, atime, mtime); err != nil {
			return err.(*os.PathError).Err.(syscall.Errno)
		}
	}

	var st syscall.Stat_t
	if err := syscall.Lstat(p, &st); err != nil {
		return err.(syscall.Errno)
	}
	fillAttr(&st, &out.Attr)
	return 0
}

// --- NodeReaddirer ---

var _ fs.NodeReaddirer = (*FuseNode)(nil)

func (n *FuseNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	entries, err := os.ReadDir(n.pathOf())
	if err != nil {
		return nil, err.(*os.PathError).Err.(syscall.Errno)
	}

	var list []gofuse.DirEntry
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		// Retrieve POSIX mode via Sys() to get correct file-type bits.
		st := info.Sys().(*syscall.Stat_t)
		list = append(list, gofuse.DirEntry{
			Mode: st.Mode,
			Name: e.Name(),
			Ino:  inodeFor(n.childRel(e.Name())),
		})
	}
	return fs.NewListDirStream(list), 0
}

// --- NodeOpener ---

var _ fs.NodeOpener = (*FuseNode)(nil)

func (n *FuseNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	f, err := os.OpenFile(n.pathOf(), int(flags), 0)
	if err != nil {
		return nil, 0, err.(*os.PathError).Err.(syscall.Errno)
	}
	return &fileHandle{f: f}, 0, 0
}

// --- NodeCreater ---

var _ fs.NodeCreater = (*FuseNode)(nil)

func (n *FuseNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *gofuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	rel := n.childRel(name)
	fullPath := filepath.Join(n.root.backingDir, rel)

	f, err := os.OpenFile(fullPath, int(flags)|os.O_CREATE, os.FileMode(mode))
	if err != nil {
		return nil, nil, 0, err.(*os.PathError).Err.(syscall.Errno)
	}

	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		f.Close()
		return nil, nil, 0, err.(syscall.Errno)
	}
	fillAttr(&st, &out.Attr)

	child := n.NewInode(ctx, &FuseNode{rel: rel, root: n.root}, fs.StableAttr{
		Mode: st.Mode,
		Ino:  inodeFor(rel),
	})
	return child, &fileHandle{f: f}, 0, 0
}

// --- NodeUnlinker ---

var _ fs.NodeUnlinker = (*FuseNode)(nil)

func (n *FuseNode) Unlink(ctx context.Context, name string) syscall.Errno {
	rel := n.childRel(name)
	if err := os.Remove(filepath.Join(n.root.backingDir, rel)); err != nil {
		return err.(*os.PathError).Err.(syscall.Errno)
	}
	return 0
}

// --- NodeMkdirer ---

var _ fs.NodeMkdirer = (*FuseNode)(nil)

func (n *FuseNode) Mkdir(ctx context.Context, name string, mode uint32, out *gofuse.EntryOut) (*fs.Inode, syscall.Errno) {
	rel := n.childRel(name)
	fullPath := filepath.Join(n.root.backingDir, rel)

	if err := os.Mkdir(fullPath, os.FileMode(mode)); err != nil {
		return nil, err.(*os.PathError).Err.(syscall.Errno)
	}

	var st syscall.Stat_t
	if err := syscall.Lstat(fullPath, &st); err != nil {
		return nil, err.(syscall.Errno)
	}
	fillAttr(&st, &out.Attr)
	// out.Attr.Mode must include S_IFDIR (from Stat_t.Mode).
	// The go-fuse bridge asserts: out.Attr.Mode &^ 07777 == syscall.S_IFDIR.

	child := n.NewInode(ctx, &FuseNode{rel: rel, root: n.root}, fs.StableAttr{
		Mode: st.Mode,
		Ino:  inodeFor(rel),
	})
	return child, 0
}

// --- NodeRmdirer ---

var _ fs.NodeRmdirer = (*FuseNode)(nil)

func (n *FuseNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	rel := n.childRel(name)
	if err := os.Remove(filepath.Join(n.root.backingDir, rel)); err != nil {
		return err.(*os.PathError).Err.(syscall.Errno)
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
	oldPath := filepath.Join(n.root.backingDir, n.childRel(name))
	newPath := filepath.Join(n.root.backingDir, newParentNode.childRel(newName))
	if err := os.Rename(oldPath, newPath); err != nil {
		return err.(*os.PathError).Err.(syscall.Errno)
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
		return uint32(n), err.(*os.PathError).Err.(syscall.Errno)
	}
	return uint32(n), 0
}

func (fh *fileHandle) Release(ctx context.Context) syscall.Errno {
	if err := fh.f.Close(); err != nil {
		return err.(*os.PathError).Err.(syscall.Errno)
	}
	return 0
}
