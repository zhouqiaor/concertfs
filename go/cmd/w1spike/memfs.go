// W1 闸门 spike：纯内存文件系统（cgofuse FileSystemInterface 实现）。
//
// 语义对齐 WinFsp FUSE 层（FUSE2 on Windows）与 PhoneFS SPEC §5.10 预测：
//   - 大小写不敏感解析（SetCapCaseInsensitive(true)），保留创建时的标准大小写；
//   - Create/Open 正确处理 O_CREAT/O_EXCL/O_TRUNC（issue #66 的常量混用已在 v1.6.0 nocgo 对齐 CRT）；
//   - Setxattr 对任意 EA（含 Defender 的 $KERNEL.PURGE.SEC.FILEHASH）返回成功——
//     issue #66 的 Explorer 复制失败根因，ENOSYS 会被翻译成 STATUS_EAS_NOT_SUPPORTED 进而报错；
//   - Unlink 走 POSIX「目录项摘除、打开句柄续命」语义，匹配 WinFsp delete-on-cleanup；
//   - Rename 允许覆盖同名普通文件（POSIX 原子替换），目录仅允许覆盖空目录。
package main

import (
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/winfsp/cgofuse/fuse"
)

const (
	dirMode  = fuse.S_IFDIR | 0o777
	fileMode = fuse.S_IFREG | 0o666
	maxName  = 255
)

// w1node 是内存树节点；data 仅对普通文件有效，chld 仅对目录有效。
type w1node struct {
	stat fuse.Stat_t
	data []byte
	chld map[string]*w1node
	open int // 打开句柄计数；>0 时即便已 unlink 也继续存活
}

func newNode(ino uint64, mode uint32) *w1node {
	now := fuse.Now()
	n := &w1node{
		stat: fuse.Stat_t{
			Ino:      ino,
			Mode:     mode,
			Nlink:    1,
			Atim:     now,
			Mtim:     now,
			Ctim:     now,
			Birthtim: now,
			Blksize:  4096,
		},
	}
	if mode&fuse.S_IFMT == fuse.S_IFDIR {
		n.chld = map[string]*w1node{}
		n.stat.Nlink = 2
	}
	return n
}

type w1memfs struct {
	fuse.FileSystemBase // xattr/link/symlink 等未实现回调默认 -ENOSYS（Setxattr 例外覆写）

	mu      sync.Mutex
	log     *levelLogger
	trace   bool
	capBlk  uint64 // statfs 总块数（4KiB/块）
	inoSeq  uint64
	root    *w1node
	openmap map[uint64]*w1node
}

func newMemfs(log *levelLogger, trace bool, capacityBytes int64) *w1memfs {
	fs := &w1memfs{
		log:     log,
		trace:   trace,
		capBlk:  uint64(capacityBytes / 4096),
		openmap: map[uint64]*w1node{},
	}
	fs.inoSeq = 1
	fs.root = newNode(fs.inoSeq, dirMode)
	return fs
}

// ---------- 路径解析（大小写不敏感） ----------

func splitPath(path string) []string {
	out := []string{}
	for _, c := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}

// lookup 返回（父节点, 标准名, 节点）；不存在时 node 为 nil。
// 根路径返回 (nil, "", root)。Windows 侧大小写不敏感。
func (fs *w1memfs) lookup(path string) (*w1node, string, *w1node) {
	comps := splitPath(path)
	parent := (*w1node)(nil)
	name := ""
	node := fs.root
	for _, c := range comps {
		parent, name = node, c
		node = fs.childCI(node, c)
	}
	return parent, name, node
}

func (fs *w1memfs) childCI(dir *w1node, name string) *w1node {
	if n, ok := dir.chld[name]; ok { // 快路径：精确命中
		return n
	}
	for k, v := range dir.chld { // 慢路径：大小写不敏感
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return nil
}

// ---------- 句柄管理 ----------

func (fs *w1memfs) ref(node *w1node) uint64 {
	node.open++
	fs.openmap[node.stat.Ino] = node
	return node.stat.Ino
}

func (fs *w1memfs) unref(fh uint64) int {
	node := fs.openmap[fh]
	if node == nil {
		return -fuse.EBADF
	}
	node.open--
	if node.open <= 0 {
		delete(fs.openmap, fh)
	}
	return 0
}

func (fs *w1memfs) nodeBy(path string, fh uint64) *w1node {
	if fh != ^uint64(0) {
		return fs.openmap[fh] // 已 unlink 的打开文件仍可经 fh 访问
	}
	_, _, n := fs.lookup(path)
	return n
}

// ---------- 生命周期 / 容量 ----------

func (fs *w1memfs) Init() {
	fs.log.Infof("Init: memfs ready (case-insensitive, capacity=%d blocks)", fs.capBlk)
}

func (fs *w1memfs) Destroy() {
	fs.log.Infof("Destroy: memfs torn down (open handles=%d)", len(fs.openmap))
}

func (fs *w1memfs) Statfs(path string, stat *fuse.Statfs_t) (errc int) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	stat.Bsize = 4096
	stat.Frsize = 4096
	stat.Blocks = fs.capBlk
	stat.Bfree = fs.capBlk
	stat.Bavail = fs.capBlk
	stat.Files = 1_000_000
	stat.Ffree = 1_000_000
	stat.Favail = 1_000_000
	stat.Namemax = maxName
	if fs.trace {
		fs.log.Tracef("Statfs path=%q -> blocks=%d", path, fs.capBlk)
	}
	return 0
}

// ---------- 节点创建 / 删除 ----------

func (fs *w1memfs) Mknod(path string, mode uint32, dev uint64) (errc int) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.makeNode(path, fuse.S_IFREG|mode&0o7777)
}

func (fs *w1memfs) Mkdir(path string, mode uint32) (errc int) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.makeNode(path, fuse.S_IFDIR|mode&0o7777)
}

func (fs *w1memfs) makeNode(path string, mode uint32) int {
	if utf8.RuneCountInString(path) > 32767 { // 兜底，正常走组件名校验
		return -fuse.ENAMETOOLONG
	}
	parent, name, node := fs.lookup(path)
	if parent == nil {
		return -fuse.ENOENT
	}
	if node != nil {
		return -fuse.EEXIST
	}
	if utf8.RuneCountInString(name) > maxName {
		return -fuse.ENAMETOOLONG
	}
	fs.inoSeq++
	n := newNode(fs.inoSeq, mode)
	parent.chld[name] = n
	now := fuse.Now()
	parent.stat.Ctim, parent.stat.Mtim = now, now
	if fs.trace {
		fs.log.Tracef("makeNode path=%q mode=%#o ino=%d", path, mode, fs.inoSeq)
	}
	return 0
}

func (fs *w1memfs) Unlink(path string) (errc int) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.removeNode(path, false)
}

func (fs *w1memfs) Rmdir(path string) (errc int) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.removeNode(path, true)
}

func (fs *w1memfs) removeNode(path string, wantDir bool) int {
	parent, name, node := fs.lookup(path)
	if node == nil {
		return -fuse.ENOENT
	}
	isDir := node.stat.Mode&fuse.S_IFMT == fuse.S_IFDIR
	if wantDir && !isDir {
		return -fuse.ENOTDIR
	}
	if !wantDir && isDir {
		return -fuse.EISDIR
	}
	if isDir && len(node.chld) > 0 {
		return -fuse.ENOTEMPTY
	}
	// POSIX 语义：直接摘除目录项；open>0 时数据随句柄存活到最后 Release。
	delete(parent.chld, name)
	node.stat.Nlink--
	now := fuse.Now()
	parent.stat.Ctim, parent.stat.Mtim = now, now
	if fs.trace {
		fs.log.Tracef("removeNode path=%q wantDir=%v openHandles=%d", path, wantDir, node.open)
	}
	return 0
}

// ---------- Rename（含覆盖替换） ----------

func (fs *w1memfs) Rename(oldpath string, newpath string) (errc int) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	oldParent, oldName, oldNode := fs.lookup(oldpath)
	if oldNode == nil {
		return -fuse.ENOENT
	}
	newParent, newName, newNode := fs.lookup(newpath)
	if newParent == nil || newName == "" {
		return -fuse.ENOENT
	}
	if utf8.RuneCountInString(newName) > maxName {
		return -fuse.ENAMETOOLONG
	}
	if oldNode == newNode { // 仅大小写改名
		delete(oldParent.chld, oldName)
		newParent.chld[newName] = oldNode
		return 0
	}
	if newNode != nil {
		oldIsDir := oldNode.stat.Mode&fuse.S_IFMT == fuse.S_IFDIR
		newIsDir := newNode.stat.Mode&fuse.S_IFMT == fuse.S_IFDIR
		switch {
		case oldIsDir != newIsDir:
			if newIsDir {
				return -fuse.EISDIR
			}
			return -fuse.ENOTDIR
		case newIsDir && len(newNode.chld) > 0:
			return -fuse.ENOTEMPTY // POSIX 只允许覆盖空目录
		case newNode.open > 0:
			return -fuse.EBUSY // 目标正被打开（Explorer 通常先关后换，见到即记录）
		}
		newNode.stat.Nlink--
		delete(newParent.chld, newName)
	}
	delete(oldParent.chld, oldName)
	newParent.chld[newName] = oldNode
	now := fuse.Now()
	oldParent.stat.Ctim, oldParent.stat.Mtim = now, now
	if newParent != oldParent {
		newParent.stat.Ctim, newParent.stat.Mtim = now, now
	}
	if fs.trace {
		fs.log.Tracef("Rename %q -> %q", oldpath, newpath)
	}
	return 0
}

// ---------- 属性 / 权限 / 时间 ----------

func (fs *w1memfs) Chmod(path string, mode uint32) (errc int) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	_, _, node := fs.lookup(path)
	if node == nil {
		return -fuse.ENOENT
	}
	node.stat.Mode = node.stat.Mode&fuse.S_IFMT | mode&0o7777
	node.stat.Ctim = fuse.Now()
	if fs.trace {
		fs.log.Tracef("Chmod path=%q mode=%#o", path, mode)
	}
	return 0
}

func (fs *w1memfs) Chflags(path string, flags uint32) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	_, _, node := fs.lookup(path)
	if node == nil {
		return -fuse.ENOENT
	}
	node.stat.Flags = flags
	node.stat.Ctim = fuse.Now()
	if fs.trace {
		fs.log.Tracef("Chflags path=%q flags=%#x", path, flags)
	}
	return 0
}

// Chown 在 uid=-1/gid=-1 挂载下无意义，成功即可（避免 ENOSYS 噪声）。
func (fs *w1memfs) Chown(path string, uid uint32, gid uint32) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	_, _, node := fs.lookup(path)
	if node == nil {
		return -fuse.ENOENT
	}
	if uid != ^uint32(0) {
		node.stat.Uid = uid
	}
	if gid != ^uint32(0) {
		node.stat.Gid = gid
	}
	node.stat.Ctim = fuse.Now()
	return 0
}

func (fs *w1memfs) Setcrtime(path string, tmsp fuse.Timespec) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	_, _, node := fs.lookup(path)
	if node == nil {
		return -fuse.ENOENT
	}
	node.stat.Birthtim = tmsp
	return 0
}

func (fs *w1memfs) Setchgtime(path string, tmsp fuse.Timespec) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	_, _, node := fs.lookup(path)
	if node == nil {
		return -fuse.ENOENT
	}
	node.stat.Ctim = tmsp
	return 0
}

func (fs *w1memfs) Utimens(path string, tmsp []fuse.Timespec) (errc int) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	_, _, node := fs.lookup(path)
	if node == nil {
		return -fuse.ENOENT
	}
	if tmsp == nil { // utimes(NULL)：设为当前时间
		now := fuse.Now()
		node.stat.Atim, node.stat.Mtim, node.stat.Ctim = now, now, now
		return 0
	}
	node.stat.Atim = tmsp[0]
	node.stat.Mtim = tmsp[1]
	if fs.trace {
		fs.log.Tracef("Utimens path=%q atim=%d mtim=%d", path, tmsp[0].Sec, tmsp[1].Sec)
	}
	return 0
}

func (fs *w1memfs) Access(path string, mask uint32) (errc int) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	_, _, node := fs.lookup(path)
	if node == nil {
		return -fuse.ENOENT
	}
	// DELETE_OK(WinFsp=0x10000? 见 fuse_common) 不在 W1 门禁用例，未注册 SetCapDeleteAccess。
	if mask&fuse.W_OK != 0 && node.stat.Mode&0o222 == 0 {
		return -fuse.EACCES
	}
	return 0
}

// ---------- 打开 / 读写 ----------

// Create 语义（POSIX O_CREAT 组合，见 issue #66 与 WinFsp fuse_intf.c）：
//   - 不存在：建新文件并返回句柄；
//   - 已存在且带 O_EXCL：-EEXIST（WinFsp 原生 Create 走这条）；
//   - 已存在无 O_EXCL：按既有文件打开，O_TRUNC 时清空（防御性，标准 FUSE 走 Open）。
func (fs *w1memfs) Create(path string, flags int, mode uint32) (errc int, fh uint64) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	_, _, existing := fs.lookup(path)
	if existing != nil {
		if flags&fuse.O_EXCL != 0 {
			if fs.trace {
				fs.log.Tracef("Create path=%q flags=%#x -> EEXIST", path, flags)
			}
			return -fuse.EEXIST, ^uint64(0)
		}
		if flags&fuse.O_TRUNC != 0 {
			existing.data = nil
			existing.stat.Size = 0
			existing.stat.Mtim = fuse.Now()
		}
		if fs.trace {
			fs.log.Tracef("Create(existing) path=%q flags=%#x fh=%d", path, flags, existing.stat.Ino)
		}
		return 0, fs.ref(existing)
	}
	if errc = fs.makeNode(path, fuse.S_IFREG|mode&0o7777); errc != 0 {
		return errc, ^uint64(0)
	}
	_, _, node := fs.lookup(path)
	fs.log.Infof("Create path=%q flags=%#x mode=%#o fh=%d", path, flags, mode, node.stat.Ino)
	return 0, fs.ref(node)
}

func (fs *w1memfs) Open(path string, flags int) (errc int, fh uint64) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	_, _, node := fs.lookup(path)
	if node == nil {
		return -fuse.ENOENT, ^uint64(0)
	}
	if node.stat.Mode&fuse.S_IFMT == fuse.S_IFDIR {
		return -fuse.EISDIR, ^uint64(0)
	}
	if flags&fuse.O_TRUNC != 0 {
		node.data = nil
		node.stat.Size = 0
		node.stat.Mtim = fuse.Now()
	}
	if fs.trace {
		fs.log.Tracef("Open path=%q flags=%#x fh=%d", path, flags, node.stat.Ino)
	}
	return 0, fs.ref(node)
}

func (fs *w1memfs) Getattr(path string, stat *fuse.Stat_t, fh uint64) (errc int) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	node := fs.nodeBy(path, fh)
	if node == nil {
		return -fuse.ENOENT
	}
	*stat = node.stat
	stat.Blocks = (stat.Size + 511) / 512
	return 0
}

func (fs *w1memfs) Truncate(path string, size int64, fh uint64) (errc int) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	node := fs.nodeBy(path, fh)
	if node == nil {
		return -fuse.ENOENT
	}
	if size < 0 {
		return -fuse.EINVAL
	}
	if int64(len(node.data)) < size {
		node.data = append(node.data, make([]byte, size-int64(len(node.data)))...)
	} else {
		node.data = node.data[:size]
	}
	node.stat.Size = size
	now := fuse.Now()
	node.stat.Mtim, node.stat.Ctim = now, now
	if fs.trace {
		fs.log.Tracef("Truncate path=%q size=%d fh=%d", path, size, fh)
	}
	return 0
}

func (fs *w1memfs) Read(path string, buff []byte, ofst int64, fh uint64) (n int) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	node := fs.nodeBy(path, fh)
	if node == nil {
		return -fuse.ENOENT
	}
	if ofst >= node.stat.Size || ofst < 0 {
		return 0
	}
	end := ofst + int64(len(buff))
	if end > node.stat.Size {
		end = node.stat.Size
	}
	n = copy(buff, node.data[ofst:end])
	node.stat.Atim = fuse.Now()
	return n
}

func (fs *w1memfs) Write(path string, buff []byte, ofst int64, fh uint64) (n int) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	node := fs.nodeBy(path, fh)
	if node == nil {
		return -fuse.ENOENT
	}
	if ofst < 0 {
		return -fuse.EINVAL
	}
	end := ofst + int64(len(buff))
	if int64(len(node.data)) < end {
		node.data = append(node.data, make([]byte, end-int64(len(node.data)))...)
	}
	n = copy(node.data[ofst:end], buff)
	node.stat.Size = int64(len(node.data))
	now := fuse.Now()
	node.stat.Mtim, node.stat.Ctim = now, now
	if fs.trace {
		fs.log.Tracef("Write path=%q ofst=%d len=%d size=%d", path, ofst, n, node.stat.Size)
	}
	return n
}

func (fs *w1memfs) Flush(path string, fh uint64) int {
	if fs.trace {
		fs.mu.Lock()
		fs.log.Tracef("Flush path=%q fh=%d", path, fh)
		fs.mu.Unlock()
	}
	return 0 // 内存 FS 无脏页
}

func (fs *w1memfs) Release(path string, fh uint64) (errc int) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.trace {
		fs.log.Tracef("Release path=%q fh=%d", path, fh)
	}
	return fs.unref(fh)
}

func (fs *w1memfs) Fsync(path string, datasync bool, fh uint64) int { return 0 }

// ---------- 目录 ----------

func (fs *w1memfs) Opendir(path string) (errc int, fh uint64) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	_, _, node := fs.lookup(path)
	if node == nil {
		return -fuse.ENOENT, ^uint64(0)
	}
	if node.stat.Mode&fuse.S_IFMT != fuse.S_IFDIR {
		return -fuse.ENOTDIR, ^uint64(0)
	}
	return 0, fs.ref(node)
}

func (fs *w1memfs) Readdir(path string,
	fill func(name string, stat *fuse.Stat_t, ofst int64) bool,
	ofst int64, fh uint64) (errc int) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	node := fs.openmap[fh]
	if node == nil {
		_, _, node = fs.lookup(path)
	}
	if node == nil {
		return -fuse.ENOENT
	}
	if !fill(".", &node.stat, 0) {
		return 0
	}
	if !fill("..", nil, 0) {
		return 0
	}
	for name, chld := range node.chld {
		st := chld.stat // 拷贝，fill 出锁后仍安全（cgofuse 当次调用内消费）
		if !fill(name, &st, 0) {
			break
		}
	}
	return 0
}

func (fs *w1memfs) Releasedir(path string, fh uint64) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.unref(fh)
}

func (fs *w1memfs) Fsyncdir(path string, datasync bool, fh uint64) int { return 0 }

// ---------- xattr：#66 关键兼容层 ----------

// Setxattr 必须成功：Windows Defender 等会在 Create 时携带
// $KERNEL.PURGE.SEC.FILEHASH 之类 EA；返回 ENOSYS 会让 Explorer 原生复制整体失败。
func (fs *w1memfs) Setxattr(path string, name string, value []byte, flags int) int {
	if fs.trace {
		fs.log.Tracef("Setxattr path=%q name=%q len=%d flags=%d -> 0 (accepted)", path, name, len(value), flags)
	}
	return 0
}

func (fs *w1memfs) Getxattr(path string, name string) (int, []byte) {
	return -fuse.ENOATTR, nil
}

func (fs *w1memfs) Removexattr(path string, name string) int { return 0 }

func (fs *w1memfs) Listxattr(path string, fill func(name string) bool) int { return 0 }

// 编译期断言：可选接口（Windows 专属）
var (
	_ fuse.FileSystemChflags    = (*w1memfs)(nil)
	_ fuse.FileSystemSetcrtime  = (*w1memfs)(nil)
	_ fuse.FileSystemSetchgtime = (*w1memfs)(nil)
)
