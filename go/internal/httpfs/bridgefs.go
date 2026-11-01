// bridgefs.go 把 httpfs.Client 适配为 cgofuse 文件系统。
// M1 保留 -ro 只读路径；M2 默认 rw，以本地临时文件承接随机写并在
// Flush/Fsync/Release 用 1MiB Content-Range 分块上传。
//
// 路径模型：FUSE 路径 "/内部存储/PhoneFS-w2/test10mb.bin"。盘符根是虚拟目录，
// 其 8 个子项直接铺服务端 /v1/roots；分类根（cat:*）在 W2 列表返回 501，
// 按空目录呈现（只记一次日志）。
//
// 读路径：文件按 1MiB 对齐块做 Range 请求，每打开句柄最多缓存 16 块（16MiB）
// LRU，绝不整文件下载；大小来自 list 的 size 属性，越界读返回 0（EOF）。
package httpfs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	pathpkg "path"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/winfsp/cgofuse/fuse"
)

const (
	blockSize         int64 = 1 << 20 // Range 读块：1MiB
	maxBlocksPerFH          = 16      // 每句柄块缓存上限（≈16MiB）
	dirTTL                  = 2 * time.Second
	readdirMinRefresh       = 250 * time.Millisecond // 强制刷新也留 250ms 节流，吸收 F5/缓冲续读突发
	statfsTTL               = 10 * time.Second
	statfsNegTTL            = 3 * time.Second
	maxNameLen              = 255
)

// 合成根 ino=1；动态节点统一从 inoDynamicStart 起分配，便于日志辨认。
const inoDynamicStart = 100

var (
	errPastEOF = errors.New("range past EOF")
)

// node 是路径缓存节点；身份（ino）按 FUSE 路径稳定分配。
type node struct {
	id       string
	name     string
	isDir    bool
	size     int64
	mod      time.Time
	writable bool
	ino      uint64
	version  uint64 // 写/改名后递增；其它读句柄据此废弃旧 Range 块
}

type dirCache struct {
	entries []*node
	at      time.Time
}

type openFile struct {
	n        *node
	mu       sync.Mutex // 串行化同句柄读写/上传（WinFsp 可能并发下发回调）
	blocks   map[int64][]byte
	lru      []int64
	cacheVer uint64

	// rw 句柄：原始内容按 1MiB Range 落入本地临时文件，随机写只改 spool。
	spool     *os.File
	spoolPath string
	size      int64
	mod       time.Time
	dirty     bool
	uploadID  string // 失败重试沿用；每次新脏写周期更换
}

// BridgeFS 实现 fuse.FileSystemInterface 的只读桥。
type BridgeFS struct {
	fuse.FileSystemBase // 未实现回调默认 -ENOSYS（Setxattr 等见下覆写）

	cl       *Client
	log      Logger
	trace    bool
	readOnly bool
	spoolDir string

	mountTime time.Time
	root      *node

	mu      sync.Mutex
	inoSeq  uint64
	byPath  map[string]*node // 内部根路径为 ""
	dirs    map[string]*dirCache
	skipped map[string]bool // 非法名/501 空分类的「只告警一次」

	sfMu   sync.Mutex
	sfAt   time.Time
	sfInfo FSInfo
	sfOK   bool

	fhMu  sync.Mutex
	fhSeq uint64
	files map[uint64]*openFile
}

// NewBridgeFS 创建桥文件系统；随后必须 Warmup 再挂载。readOnly=true 完整保留 M1 行为。
func NewBridgeFS(cl *Client, log Logger, trace, readOnly bool) *BridgeFS {
	spoolDir := ""
	if !readOnly {
		// 每挂载会话独立目录；Destroy 清理。创建失败在首次写打开时显式返回 ENOSPC。
		spoolDir, _ = os.MkdirTemp("", "phonefs-m2-")
	}
	b := &BridgeFS{
		cl:        cl,
		log:       log,
		trace:     trace,
		readOnly:  readOnly,
		spoolDir:  spoolDir,
		mountTime: time.Now(),
		byPath:    map[string]*node{},
		dirs:      map[string]*dirCache{},
		skipped:   map[string]bool{},
		files:     map[uint64]*openFile{},
		inoSeq:    inoDynamicStart,
	}
	b.root = &node{name: "/", isDir: true, ino: 1}
	b.byPath[""] = b.root
	return b
}

// Warmup 挂载前预检：roots 必须成功（token/网络错误快速暴露），
// statfs 失败不致命（运行期会再取）。pair/info 由 main 单独取（卷标用）。
func (b *BridgeFS) Warmup(ctx context.Context) error {
	// 直接走客户端：预检要把 401 原始错误抛给 main 给明确提示，不经过 errno 映射。
	roots, err := b.cl.Roots(ctx)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.dirs[""] = &dirCache{entries: b.storeEntriesLocked("", roots), at: time.Now()}
	b.mu.Unlock()
	if info, err := b.cl.StatFS(ctx); err == nil {
		b.sfMu.Lock()
		b.sfAt, b.sfInfo, b.sfOK = time.Now(), info, true
		b.sfMu.Unlock()
	} else {
		b.log.Warnf("statfs preflight failed (non-fatal): %v", err)
	}
	return nil
}

// ---------- 路径解析 / 目录缓存 ----------

func splitComps(path string) []string {
	out := []string{}
	for _, c := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}

// walkLocked 按组件逐级查 byPath（快路径精确、慢路径 EqualFold），
// 返回命中节点、第一个缺失组件下标、已命中前缀的规范路径。
// 全部命中时 missAt=len(comps)，canon 为完整规范路径。调用方持 b.mu。
func (b *BridgeFS) walkLocked(comps []string) (n *node, missAt int, canon string) {
	cur := b.root
	canon = ""
	for i, c := range comps {
		next := b.byPath[canon+"/"+c]
		if next == nil { // 大小写不敏感：扫父目录缓存
			for _, ch := range b.dirs[canon].entriesOrNil() {
				if strings.EqualFold(ch.name, c) {
					next = ch
					break
				}
			}
		}
		if next == nil {
			return cur, i, canon
		}
		cur = next
		canon += "/" + next.name // 始终用服务端规范名，保证下级路径正确
	}
	return cur, len(comps), canon
}

// entriesOrNil 让 nil map 也能安全 range。
func (d *dirCache) entriesOrNil() []*node {
	if d == nil {
		return nil
	}
	return d.entries
}

// resolve 把 FUSE 路径解析成 node；缺缓存时列一次父目录再试，仍无 → ENOENT。
func (b *BridgeFS) resolve(path string) (*node, int) {
	comps := splitComps(path)
	if len(comps) == 0 {
		return b.root, 0
	}
	var lastErr int
	for attempt := 0; attempt < 2; attempt++ {
		b.mu.Lock()
		n, missAt, canon := b.walkLocked(comps)
		b.mu.Unlock()
		if missAt == len(comps) {
			return n, 0
		}
		// canon 是已命中部分的规范路径，即缺失组件的父目录。
		if _, errc := b.refreshDir(canon, false); errc != 0 {
			lastErr = errc
			if errc == -fuse.ENOENT {
				continue // 父目录也可能只是缓存缺失，再试一轮
			}
			return nil, errc
		}
	}
	if lastErr != 0 {
		return nil, lastErr
	}
	return nil, -fuse.ENOENT
}

// canonicalNode 同时返回缓存规范路径，写操作据此精确失效大小写不敏感节点。
func (b *BridgeFS) canonicalNode(path string) (string, *node, int) {
	n, errc := b.resolve(path)
	if errc != 0 {
		return "", nil, errc
	}
	comps := splitComps(path)
	b.mu.Lock()
	_, missAt, canon := b.walkLocked(comps)
	b.mu.Unlock()
	if missAt != len(comps) {
		return "", nil, -fuse.ENOENT
	}
	return canon, n, 0
}

// refreshDir 取目录列表并入缓存。force=true 时忽略 TTL（Readdir 走这里）。
// 返回缓存与 0/负 errno。
func (b *BridgeFS) refreshDir(dirPath string, force bool) (*dirCache, int) {
	b.mu.Lock()
	if dc := b.dirs[dirPath]; dc != nil {
		age := time.Since(dc.at)
		if (!force && age < dirTTL) || (force && age < readdirMinRefresh) {
			b.mu.Unlock()
			return dc, 0
		}
	}
	var parentID string
	if dirPath != "" {
		if n := b.byPath[dirPath]; n != nil {
			parentID = n.id
		} else {
			b.mu.Unlock()
			return nil, -fuse.ENOENT
		}
	}
	b.mu.Unlock()

	ctx := context.Background()
	var entries []Entry
	var err error
	if dirPath == "" {
		entries, err = b.cl.Roots(ctx)
	} else {
		entries, _, err = b.cl.List(ctx, parentID)
		if err != nil && IsNotSupported(err) && strings.HasPrefix(parentID, "cat:") {
			// W2 分类后端未实现：分类根按空目录呈现，Explorer 可正常进入。
			key := "cat-empty:" + parentID
			b.mu.Lock()
			first := !b.markOnceLocked(key)
			b.mu.Unlock()
			if first {
				b.log.Infof("category root %s list unsupported by W2: presenting as empty dir", parentID)
			}
			entries, err = []Entry{}, nil
		}
	}
	if err != nil {
		return nil, b.apiErrno(err, "list", dirPath)
	}

	b.mu.Lock()
	nodes := b.storeEntriesLocked(dirPath, entries)
	dc := &dirCache{entries: nodes, at: time.Now()}
	b.dirs[dirPath] = dc
	b.mu.Unlock()
	if b.trace {
		b.log.Tracef("refreshDir %q -> %d entries", slashOrRoot(dirPath), len(nodes))
	}
	return dc, 0
}

// storeEntriesLocked upsert 条目（跳过 Windows 非法名）并返回排序后子节点；调用方持 b.mu。
func (b *BridgeFS) storeEntriesLocked(dirPath string, entries []Entry) []*node {
	nodes := make([]*node, 0, len(entries))
	for i := range entries {
		e := &entries[i]
		if !legalName(e.Name) {
			key := "badname:" + dirPath + "/" + e.Name
			if !b.markOnceLocked(key) {
				b.log.Warnf("skipping Windows-illegal entry name %q in %q", e.Name, dirPath)
			}
			continue
		}
		nodes = append(nodes, b.upsertLocked(dirPath+"/"+e.Name, e))
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].isDir != nodes[j].isDir {
			return nodes[i].isDir // 目录在前
		}
		return strings.ToLower(nodes[i].name) < strings.ToLower(nodes[j].name)
	})
	return nodes
}

func (b *BridgeFS) upsertLocked(path string, e *Entry) *node {
	mod := time.Time{}
	if e.ModifiedMs > 0 {
		mod = time.UnixMilli(e.ModifiedMs)
	}
	if n, ok := b.byPath[path]; ok {
		if n.id != e.ID || n.isDir != e.IsDir || n.size != e.Size || !n.mod.Equal(mod) {
			n.version++
		}
		n.id, n.isDir, n.size, n.mod, n.writable = e.ID, e.IsDir, e.Size, mod, e.Writable
		n.name = e.Name
		return n
	}
	b.inoSeq++
	n := &node{
		id: e.ID, name: e.Name, isDir: e.IsDir, size: e.Size,
		mod: mod, writable: e.Writable, ino: b.inoSeq, version: 1,
	}
	b.byPath[path] = n
	return n
}

// markOnceLocked 首次返回 false 并登记；调用方持 b.mu。
func (b *BridgeFS) markOnceLocked(key string) bool {
	if b.skipped[key] {
		return true
	}
	b.skipped[key] = true
	return false
}

// legalName 拒绝 Windows 卷非法文件名（Android 侧允许 : 等字符，M1 先隐藏+告警，
// 不下发改名映射——那会破坏按名解析）。
func legalName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if utf8.RuneCountInString(name) > maxNameLen {
		return false
	}
	if strings.ContainsAny(name, `<>:"/\|?*`) {
		return false
	}
	if strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 }) {
		return false
	}
	last := name[len(name)-1]
	return last != '.' && last != ' '
}

func slashOrRoot(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

// ---------- errno 映射 ----------

// apiErrno 记录并映射服务端错误；409 按 code 区分冲突，507 映射 Windows 磁盘已满。
func (b *BridgeFS) apiErrno(err error, op, path string) int {
	var ae *APIError
	if errors.As(err, &ae) {
		switch ae.HTTPStatus {
		case 401, 403:
			b.log.Warnf("%s %s: auth failure: %v", op, slashOrRoot(path), ae)
			return -fuse.EACCES
		case 404:
			if b.trace {
				b.log.Tracef("%s %s: not found: %v", op, slashOrRoot(path), ae.Code)
			}
			return -fuse.ENOENT
		case 400:
			return -fuse.EINVAL
		case 409:
			switch ae.Code {
			case "ALREADY_EXISTS":
				return -fuse.EEXIST
			case "NOT_EMPTY":
				return -fuse.ENOTEMPTY
			default: // UPLOAD_INCOMPLETE / PARTIAL：调用方可重试整轮上传
				return -fuse.EIO
			}
		case 413:
			return -fuse.EFBIG
		case 416:
			return 0 // Read 路径特判；其它路径理论不可达
		case 429:
			return -fuse.EAGAIN
		case 501:
			b.log.Warnf("%s %s: not implemented: %v", op, slashOrRoot(path), ae)
			return -fuse.ENOSYS
		case 507:
			return -fuse.ENOSPC
		}
	}
	b.log.Errorf("%s %s failed: %v", op, slashOrRoot(path), err)
	return -fuse.EIO
}

// ---------- FUSE 生命周期 / 容量 ----------

// Init 由 cgofuse 在挂载线程启动时回调。
func (b *BridgeFS) Init() {
	mode := "rw"
	if b.readOnly {
		mode = "ro"
	}
	b.log.Infof("Init: phonefs bridge ready mode=%s (block=%dMiB cache=%d/fh)",
		mode, blockSize/(1<<20), maxBlocksPerFH)
}

func (b *BridgeFS) Destroy() {
	if b.spoolDir != "" {
		if err := os.RemoveAll(b.spoolDir); err != nil {
			b.log.Warnf("spool cleanup %q failed: %v", b.spoolDir, err)
		}
	}
	b.log.Infof("Destroy: bridge torn down (open handles=%d)", len(b.files))
}

func (b *BridgeFS) Statfs(path string, st *fuse.Statfs_t) int {
	info, err := b.cachedStatfs()
	if err != nil {
		// 服务端挂了也不让卷变成「无容量」：有旧值用旧值，否则合成 1TiB。
		b.sfMu.Lock()
		ok := b.sfOK
		info = b.sfInfo
		b.sfMu.Unlock()
		if !ok {
			b.log.Errorf("statfs unavailable and no cached value: reporting synthetic 1TiB: %v", err)
			st.Bsize, st.Frsize = 4096, 4096
			st.Blocks, st.Bfree, st.Bavail = 1<<28, 1<<28, 1<<28 // 1TiB = 2^28 块 × 4096B
		} else {
			b.log.Warnf("statfs refresh failed: serving stale capacity: %v", err)
			b.fillStatfs(st, info)
		}
	} else {
		b.fillStatfs(st, info)
	}
	st.Files, st.Ffree, st.Favail = 1_000_000_000, 1_000_000_000, 1_000_000_000
	st.Namemax = maxNameLen
	if b.trace {
		b.log.Tracef("Statfs %q -> total=%d free=%d bs=%d", path, info.TotalBytes, info.FreeBytes, info.BlockSize)
	}
	return 0
}

func (b *BridgeFS) fillStatfs(st *fuse.Statfs_t, info FSInfo) {
	bs := uint64(info.BlockSize)
	if bs == 0 {
		bs = 4096
	}
	st.Bsize, st.Frsize = bs, bs
	st.Blocks = uint64(info.TotalBytes) / bs
	st.Bfree = uint64(info.FreeBytes) / bs
	st.Bavail = st.Bfree
}

func (b *BridgeFS) cachedStatfs() (FSInfo, error) {
	b.sfMu.Lock()
	age := time.Since(b.sfAt)
	info, ok := b.sfInfo, b.sfOK
	need := (!ok && age > statfsNegTTL) || (ok && age > statfsTTL)
	b.sfMu.Unlock()
	if !need {
		if ok {
			return info, nil
		}
		// 负缓存窗口内：不打服务端，但仍要给上层一个错误
		return FSInfo{}, errors.New("statfs in negative-cache window")
	}
	fresh, err := b.cl.StatFS(context.Background())
	b.sfMu.Lock()
	if err == nil {
		b.sfAt, b.sfInfo, b.sfOK = time.Now(), fresh, true
	} else if !ok {
		b.sfAt = time.Now() // 仅刷新负缓存时间戳
	}
	b.sfMu.Unlock()
	if err != nil {
		return FSInfo{}, err
	}
	return fresh, nil
}

// ---------- 属性 ----------

// fillAttr 把节点属性拷进 FUSE stat；节点字段会被目录刷新就地更新，故取锁拷贝。
func (b *BridgeFS) fillAttr(n *node, st *fuse.Stat_t) {
	b.mu.Lock()
	ino, isDir, size := n.ino, n.isDir, n.size
	mt := n.mod
	b.mu.Unlock()
	st.Ino = ino
	st.Uid, st.Gid = 0, 0
	st.Blksize = 4096
	t := fuse.NewTimespec(b.effTime(mt))
	st.Atim, st.Mtim, st.Ctim, st.Birthtim = t, t, t, t
	if isDir {
		st.Mode = fuse.S_IFDIR | 0o755
		if b.readOnly {
			st.Mode = fuse.S_IFDIR | 0o555
		}
		st.Nlink = 2
		return
	}
	st.Mode = fuse.S_IFREG | 0o644
	if b.readOnly {
		st.Mode = fuse.S_IFREG | 0o444
	}
	st.Nlink = 1
	st.Size = size
	st.Blocks = (size + 511) / 512
}

func (b *BridgeFS) effTime(t time.Time) time.Time {
	if t.IsZero() {
		return b.mountTime // 分类根 modified_ms=0：用挂载时间，避免显示 1970
	}
	return t
}

func (b *BridgeFS) Getattr(path string, st *fuse.Stat_t, fh uint64) int {
	if f := b.fileByFH(fh); f != nil {
		f.mu.Lock()
		defer f.mu.Unlock()
		b.fillAttr(f.n, st)
		if f.spool != nil {
			st.Size = f.size
			st.Blocks = (f.size + 511) / 512
			t := fuse.NewTimespec(b.effTime(f.mod))
			st.Atim, st.Mtim, st.Ctim, st.Birthtim = t, t, t, t
		}
		return 0
	}
	n, errc := b.resolve(path)
	if errc != 0 {
		return errc
	}
	b.fillAttr(n, st)
	return 0
}

// ---------- 打开 / 读 ----------

func (b *BridgeFS) Open(path string, flags int) (int, uint64) {
	n, errc := b.resolve(path)
	if errc != 0 {
		return errc, ^uint64(0)
	}
	if n.isDir {
		return -fuse.EISDIR, ^uint64(0)
	}
	writeIntent := flags&fuse.O_ACCMODE != fuse.O_RDONLY || flags&(fuse.O_CREAT|fuse.O_TRUNC) != 0
	if b.readOnly && writeIntent {
		b.log.Warnf("write-open refused on read-only bridge: %q flags=%#x", path, flags)
		return -fuse.EROFS, ^uint64(0)
	}
	f := &openFile{n: n, blocks: map[int64][]byte{}, cacheVer: n.version, size: n.size, mod: n.mod}
	if writeIntent {
		if errc := b.initSpool(f, flags&fuse.O_TRUNC != 0); errc != 0 {
			return errc, ^uint64(0)
		}
	}
	fh := b.registerFile(f)
	if b.trace {
		b.log.Tracef("Open %q size=%d fh=%d spool=%v", path, n.size, fh, f.spool != nil)
	}
	return 0, fh
}

func (b *BridgeFS) registerFile(f *openFile) uint64 {
	b.fhMu.Lock()
	defer b.fhMu.Unlock()
	b.fhSeq++
	b.files[b.fhSeq] = f
	return b.fhSeq
}

// initSpool 创建本地随机写副本；原始内容按块下载，绝不整文件进内存。
func (b *BridgeFS) initSpool(f *openFile, truncate bool) int {
	if b.spoolDir == "" {
		b.log.Errorf("no local spool directory available")
		return -fuse.ENOSPC
	}
	tmp, err := os.CreateTemp(b.spoolDir, "write-*.tmp")
	if err != nil {
		b.log.Errorf("create spool for %q failed: %v", f.n.name, err)
		return -fuse.ENOSPC
	}
	f.spool, f.spoolPath = tmp, tmp.Name()
	fail := func(errc int) int {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		f.spool, f.spoolPath = nil, ""
		return errc
	}
	for off := int64(0); off < f.size; {
		end := off + blockSize - 1
		if end >= f.size {
			end = f.size - 1
		}
		res, err := b.cl.ContentRange(context.Background(), f.n.id, off, end)
		if err != nil {
			return fail(b.apiErrno(err, "spool-read", f.n.name))
		}
		if len(res.Data) == 0 {
			return fail(-fuse.EIO)
		}
		if _, err := tmp.WriteAt(res.Data, off); err != nil {
			b.log.Errorf("spool write %q failed: %v", f.n.name, err)
			return fail(-fuse.ENOSPC)
		}
		off += int64(len(res.Data))
	}
	if truncate {
		if err := tmp.Truncate(0); err != nil {
			return fail(-fuse.EIO)
		}
		f.size, f.mod, f.dirty = 0, time.Now(), true
	}
	return 0
}

func (b *BridgeFS) Read(path string, buff []byte, ofst int64, fh uint64) int {
	if ofst < 0 {
		return -fuse.EINVAL
	}
	f := b.fileByFH(fh)
	if f == nil {
		return -fuse.EBADF
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.spool != nil {
		if ofst >= f.size {
			return 0
		}
		n, err := f.spool.ReadAt(buff, ofst)
		if err != nil && !errors.Is(err, io.EOF) {
			b.log.Errorf("spool read %q failed: %v", path, err)
			return -fuse.EIO
		}
		return n
	}

	b.mu.Lock()
	version := f.n.version
	b.mu.Unlock()
	if f.cacheVer != version {
		f.blocks = map[int64][]byte{}
		f.lru = nil
		f.cacheVer = version
	}
	n := 0
	for n < len(buff) {
		pos := ofst + int64(n)
		b.mu.Lock()
		size := f.n.size
		b.mu.Unlock()
		if pos >= size {
			break // EOF（含 416 归一）
		}
		blk, err := b.fetchBlock(f, pos/blockSize)
		if err != nil {
			if errors.Is(err, errPastEOF) {
				break
			}
			errc := b.apiErrno(err, "read", path)
			if n > 0 {
				break // 已交付部分字节；短读让上层下次重试，避免整体失败
			}
			return errc
		}
		inBlk := pos - (pos/blockSize)*blockSize
		avail := int64(len(blk)) - inBlk
		if toEOF := size - pos; toEOF < avail {
			avail = toEOF
		}
		take := int64(len(buff) - n)
		if take > avail {
			take = avail
		}
		if take <= 0 {
			break
		}
		copy(buff[n:n+int(take)], blk[inBlk:inBlk+take])
		n += int(take)
	}
	return n
}

func (b *BridgeFS) Flush(path string, fh uint64) int {
	f := b.fileByFH(fh)
	if f == nil {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return b.flushLocked(path, f)
}

func (b *BridgeFS) Fsync(path string, datasync bool, fh uint64) int {
	f := b.fileByFH(fh)
	if f == nil {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.spool != nil {
		if err := f.spool.Sync(); err != nil {
			b.log.Errorf("spool sync %q failed: %v", path, err)
			return -fuse.EIO
		}
	}
	return b.flushLocked(path, f)
}

// flushLocked 上传一个脏写周期；同 Upload-Id 从头重传可覆盖「已提交但响应丢失」。
func (b *BridgeFS) flushLocked(path string, f *openFile) int {
	if f.spool == nil || !f.dirty {
		return 0
	}
	mtime := f.mod.UnixMilli()
	var err error
	if f.size == 0 {
		_, err = b.cl.WriteFull(context.Background(), f.n.id, bytes.NewReader(nil), 0, &mtime)
	} else {
		if f.uploadID == "" {
			f.uploadID, err = newUploadID()
			if err != nil {
				return -fuse.EIO
			}
		}
		for off := int64(0); off < f.size; off += blockSize {
			length := blockSize
			if remain := f.size - off; remain < length {
				length = remain
			}
			var res WriteResult
			res, err = b.cl.WriteChunk(context.Background(), f.n.id, f.uploadID, f.spool, off, length, f.size, &mtime)
			if err != nil {
				break
			}
			if off+length == f.size && !res.Complete {
				err = errors.New("final chunk acknowledged without complete=true")
				break
			}
		}
	}
	if err != nil {
		return b.apiErrno(err, "flush", path)
	}
	f.dirty, f.uploadID = false, ""
	b.mu.Lock()
	f.n.size, f.n.mod = f.size, time.UnixMilli(mtime)
	f.n.version++
	// 目录属性在下一次 Readdir/Getattr 重新拉取；其它句柄靠 version 清 Range 块。
	b.dirs = map[string]*dirCache{}
	b.mu.Unlock()
	b.sfMu.Lock()
	b.sfAt = time.Time{} // 写后容量不沿用旧 TTL
	b.sfMu.Unlock()
	if b.trace {
		b.log.Tracef("Flush %q uploaded=%d bytes fh-spool=%q", path, f.size, f.spoolPath)
	}
	return 0
}

func newUploadID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "m2-" + hex.EncodeToString(raw[:]), nil
}

func (b *BridgeFS) Release(path string, fh uint64) int {
	b.fhMu.Lock()
	f := b.files[fh]
	delete(b.files, fh)
	b.fhMu.Unlock()
	if f == nil {
		return -fuse.EBADF
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	errc := b.flushLocked(path, f)
	if f.spool != nil {
		if err := f.spool.Close(); err != nil && errc == 0 {
			errc = -fuse.EIO
		}
		if err := os.Remove(f.spoolPath); err != nil && !os.IsNotExist(err) {
			b.log.Warnf("remove spool %q failed: %v", f.spoolPath, err)
		}
	}
	if b.trace {
		b.log.Tracef("Release %q fh=%d cachedBlocks=%d errc=%d", path, fh, len(f.blocks), errc)
	}
	return errc
}

// fetchBlock 取（或命中缓存）一个对齐块；同句柄由 Read 持 f.mu 串行化。
func (b *BridgeFS) fetchBlock(f *openFile, bi int64) ([]byte, error) {
	if blk, ok := f.blocks[bi]; ok {
		f.touchLRU(bi)
		return blk, nil
	}
	start := bi * blockSize
	size := f.n.size
	end := start + blockSize - 1
	if end >= size {
		end = size - 1
	}
	if start >= size {
		return nil, errPastEOF
	}
	res, err := b.cl.ContentRange(context.Background(), f.n.id, start, end)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.HTTPStatus == 416 {
			return nil, errPastEOF
		}
		return nil, err
	}
	if res.Total >= 0 && res.Total != f.n.size {
		// M1 fixture 只读不变；真发生时仅记录（节点属性由下次 list 刷新收敛），
		// 不在这里改共享节点，避免与 fillAttr 竞争。
		b.log.Warnf("file size changed server-side for %q: cached=%d server=%d", f.n.name, f.n.size, res.Total)
		if start >= res.Total {
			return nil, errPastEOF
		}
	}
	if res.First != start {
		return nil, &APIError{HTTPStatus: 502, Code: "BAD_RANGE_ALIGN", Message: "server returned misaligned range start"}
	}
	// 存缓存并做 LRU 淘汰
	f.touchLRU(bi)
	f.blocks[bi] = res.Data
	for len(f.lru) > maxBlocksPerFH {
		victim := f.lru[0]
		f.lru = f.lru[1:]
		if victim != bi {
			delete(f.blocks, victim)
		}
	}
	if b.trace {
		b.log.Tracef("fetchBlock %q bi=%d [%d-%d] %d bytes (cached=%d)",
			f.n.name, bi, start, start+int64(len(res.Data))-1, len(res.Data), len(f.blocks))
	}
	return res.Data, nil
}

func (f *openFile) touchLRU(bi int64) {
	for i, x := range f.lru {
		if x == bi {
			f.lru = append(f.lru[:i], f.lru[i+1:]...)
			break
		}
	}
	f.lru = append(f.lru, bi)
}

// ---------- 目录 ----------

func (b *BridgeFS) Opendir(path string) (int, uint64) {
	if b.trace {
		b.log.Tracef("Opendir %q", slashOrRoot(path))
	}
	n, errc := b.resolve(path)
	if errc != 0 {
		return errc, ^uint64(0)
	}
	if !n.isDir {
		return -fuse.ENOTDIR, ^uint64(0)
	}
	return 0, 0 // 目录按路径服务，不建句柄状态
}

func (b *BridgeFS) Readdir(path string, fill func(name string, stat *fuse.Stat_t, ofst int64) bool,
	ofst int64, fh uint64) int {
	key := path // 内部键与 byPath 一致：根为 ""，其余保留前导 "/"
	if key == "/" {
		key = ""
	}
	dc, errc := b.refreshDir(key, true)
	if errc != 0 {
		return errc
	}
	self := b.root
	if key != "" {
		b.mu.Lock()
		self = b.byPath[key]
		b.mu.Unlock()
	}
	if self == nil {
		b.log.Errorf("Readdir %q: refreshed dir but self node missing (key=%q)", path, key)
		return -fuse.ENOENT
	}
	var st fuse.Stat_t
	b.fillAttr(self, &st)
	if !fill(".", &st, 0) || !fill("..", nil, 0) {
		return 0
	}
	for _, ch := range dc.entries {
		var cst fuse.Stat_t
		b.fillAttr(ch, &cst)
		if !fill(ch.name, &cst, 0) {
			break // ReaddirPlus 缓冲满；WinFsp 会带 offset 再来（M1 简单重枚举）
		}
	}
	if b.trace {
		b.log.Tracef("Readdir %q -> %d children", slashOrRoot(path), len(dc.entries))
	}
	return 0
}

func (b *BridgeFS) Releasedir(path string, fh uint64) int { return 0 }

// ---------- 访问与 Windows 兼容层 ----------

func (b *BridgeFS) Access(path string, mask uint32) int {
	// ro：写/删提前拒绝，避免 WinFsp delete-on-cleanup 假成功；rw 交给具体操作映射错误。
	if b.readOnly && mask&(fuse.W_OK|fuse.DELETE_OK) != 0 {
		if b.trace {
			b.log.Tracef("Access %q mask=%#x -> EACCES (read-only)", path, mask)
		}
		return -fuse.EACCES
	}
	if _, errc := b.resolve(path); errc != 0 {
		return errc
	}
	return 0
}

// ---------- M2 写操作；ro 模式保持 M1 的 EROFS ----------

func (b *BridgeFS) roRefused(op, path string) int {
	if b.trace {
		b.log.Tracef("%s %q -> EROFS (read-only bridge)", op, path)
	}
	return -fuse.EROFS
}

func (b *BridgeFS) Unlink(path string) int {
	if b.readOnly {
		return b.roRefused("Unlink", path)
	}
	return b.delete(path, false)
}

func (b *BridgeFS) Rmdir(path string) int {
	if b.readOnly {
		return b.roRefused("Rmdir", path)
	}
	return b.delete(path, true)
}

func (b *BridgeFS) delete(path string, wantDir bool) int {
	canon, n, errc := b.canonicalNode(path)
	if errc != 0 {
		return errc
	}
	if n.isDir != wantDir {
		if wantDir {
			return -fuse.ENOTDIR
		}
		return -fuse.EISDIR
	}
	if err := b.cl.Delete(context.Background(), n.id, false); err != nil {
		return b.apiErrno(err, "delete", path)
	}
	b.mu.Lock()
	b.removeSubtreeLocked(canon)
	b.dirs = map[string]*dirCache{}
	b.mu.Unlock()
	b.sfMu.Lock()
	b.sfAt = time.Time{}
	b.sfMu.Unlock()
	return 0
}

// removeSubtreeLocked 删除路径缓存；已打开句柄仍持有 node 指针，不受影响。
func (b *BridgeFS) removeSubtreeLocked(path string) {
	for p := range b.byPath {
		if p == path || strings.HasPrefix(p, path+"/") {
			delete(b.byPath, p)
		}
	}
	for p := range b.dirs {
		if p == path || strings.HasPrefix(p, path+"/") {
			delete(b.dirs, p)
		}
	}
}

func (b *BridgeFS) Mkdir(path string, mode uint32) int {
	if b.readOnly {
		return b.roRefused("Mkdir", path)
	}
	parentPath, name, parent, errc := b.createTarget(path)
	if errc != 0 {
		return errc
	}
	e, err := b.cl.Create(context.Background(), parent.id, name, true)
	if err != nil {
		return b.apiErrno(err, "mkdir", path)
	}
	b.mu.Lock()
	b.upsertLocked(parentPath+"/"+e.Name, &e)
	b.dirs = map[string]*dirCache{}
	b.mu.Unlock()
	return 0
}

func (b *BridgeFS) Mknod(path string, mode uint32, dev uint64) int {
	if b.readOnly {
		return b.roRefused("Mknod", path)
	}
	return -fuse.ENOSYS
}

func (b *BridgeFS) Create(path string, flags int, mode uint32) (int, uint64) {
	if b.readOnly {
		return b.roRefused("Create", path), ^uint64(0)
	}
	parentPath, name, parent, errc := b.createTarget(path)
	if errc != 0 {
		return errc, ^uint64(0)
	}
	e, err := b.cl.Create(context.Background(), parent.id, name, false)
	if err != nil {
		return b.apiErrno(err, "create", path), ^uint64(0)
	}
	b.mu.Lock()
	n := b.upsertLocked(parentPath+"/"+e.Name, &e)
	b.dirs = map[string]*dirCache{}
	b.mu.Unlock()
	f := &openFile{n: n, blocks: map[int64][]byte{}, cacheVer: n.version, size: 0, mod: n.mod}
	if errc := b.initSpool(f, false); errc != 0 {
		_ = b.cl.Delete(context.Background(), n.id, false)
		b.mu.Lock()
		b.removeSubtreeLocked(parentPath + "/" + e.Name)
		b.mu.Unlock()
		return errc, ^uint64(0)
	}
	return 0, b.registerFile(f)
}

func (b *BridgeFS) createTarget(path string) (string, string, *node, int) {
	name := pathpkg.Base(path)
	if !legalName(name) {
		return "", "", nil, -fuse.EINVAL
	}
	parent := pathpkg.Dir(path)
	if parent == "/" || parent == "." {
		parent = ""
	}
	canon, n, errc := b.canonicalNode(parent)
	if errc != 0 {
		return "", "", nil, errc
	}
	if !n.isDir {
		return "", "", nil, -fuse.ENOTDIR
	}
	if canon == "" { // 虚拟盘符根不是服务端真实父目录
		return "", "", nil, -fuse.EACCES
	}
	return canon, name, n, 0
}

func (b *BridgeFS) Write(path string, buff []byte, ofst int64, fh uint64) int {
	if b.readOnly {
		return b.roRefused("Write", path)
	}
	if ofst < 0 {
		return -fuse.EINVAL
	}
	f := b.fileByFH(fh)
	if f == nil {
		return -fuse.EBADF
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.spool == nil {
		return -fuse.EACCES
	}
	n, err := f.spool.WriteAt(buff, ofst)
	if err != nil {
		b.log.Errorf("Write spool %q failed: %v", path, err)
		return -fuse.ENOSPC
	}
	if end := ofst + int64(n); end > f.size {
		f.size = end
	}
	f.mod, f.dirty, f.uploadID = time.Now(), true, ""
	return n
}

func (b *BridgeFS) Truncate(path string, size int64, fh uint64) int {
	if b.readOnly {
		return b.roRefused("Truncate", path)
	}
	if size < 0 {
		return -fuse.EINVAL
	}
	if size != 0 { // SPEC P2-12：已有文件缩短到非零长度降 M4；M2 仅承诺截零。
		return -fuse.ENOSYS
	}
	if f := b.fileByFH(fh); f != nil {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.spool == nil {
			return -fuse.EACCES
		}
		if err := f.spool.Truncate(0); err != nil {
			return -fuse.EIO
		}
		f.size, f.mod, f.dirty, f.uploadID = 0, time.Now(), true, ""
		return 0
	}
	canon, n, errc := b.canonicalNode(path)
	if errc != 0 {
		return errc
	}
	if n.isDir {
		return -fuse.EISDIR
	}
	mtime := time.Now().UnixMilli()
	if _, err := b.cl.WriteFull(context.Background(), n.id, bytes.NewReader(nil), 0, &mtime); err != nil {
		return b.apiErrno(err, "truncate", path)
	}
	b.mu.Lock()
	n.size, n.mod = 0, time.UnixMilli(mtime)
	n.version++
	if current := b.byPath[canon]; current == nil {
		b.byPath[canon] = n
	}
	b.dirs = map[string]*dirCache{}
	b.mu.Unlock()
	return 0
}

func (b *BridgeFS) Rename(oldpath string, newpath string) int {
	if b.readOnly {
		return b.roRefused("Rename", oldpath)
	}
	oldCanon, n, errc := b.canonicalNode(oldpath)
	if errc != 0 {
		return errc
	}
	newParent, name, parent, errc := b.createTarget(newpath)
	if errc != 0 {
		return errc
	}
	e, err := b.cl.PatchMeta(context.Background(), n.id, &name, &parent.id, nil)
	if err != nil {
		return b.apiErrno(err, "rename", oldpath)
	}
	b.mu.Lock()
	delete(b.byPath, oldCanon)
	n.id, n.name, n.mod = e.ID, e.Name, time.UnixMilli(e.ModifiedMs)
	n.version++
	b.byPath[newParent+"/"+e.Name] = n
	b.dirs = map[string]*dirCache{}
	b.mu.Unlock()
	return 0
}

func (b *BridgeFS) Link(path string, newpath string) int {
	if b.readOnly {
		return b.roRefused("Link", path)
	}
	return -fuse.ENOSYS
}
func (b *BridgeFS) Symlink(target string, newpath string) int {
	if b.readOnly {
		return b.roRefused("Symlink", newpath)
	}
	return -fuse.ENOSYS
}
func (b *BridgeFS) Chmod(path string, mode uint32) int {
	if b.readOnly {
		return b.roRefused("Chmod", path)
	}
	return 0 // FAT/MES 语义：权限位空操作
}
func (b *BridgeFS) Chown(path string, uid uint32, gid uint32) int {
	if b.readOnly {
		return b.roRefused("Chown", path)
	}
	return 0
}
func (b *BridgeFS) Utimens(path string, tmsp []fuse.Timespec) int {
	if b.readOnly {
		return b.roRefused("Utimens", path)
	}
	canon, n, errc := b.canonicalNode(path)
	if errc != 0 {
		return errc
	}
	mtime := time.Now()
	if len(tmsp) >= 2 {
		mtime = tmsp[1].Time()
	}
	ms := mtime.UnixMilli()
	e, err := b.cl.PatchMeta(context.Background(), n.id, nil, nil, &ms)
	if err != nil {
		return b.apiErrno(err, "utimens", path)
	}
	b.mu.Lock()
	b.upsertLocked(canon, &e)
	b.dirs = map[string]*dirCache{}
	b.mu.Unlock()
	return 0
}

// Setxattr 必须成功：Defender 在创建/扫描路径携带 $KERNEL.PURGE.* EA，
// ENOSYS 会被 WinFsp 翻译成 STATUS_EAS_NOT_SUPPORTED 导致 Explorer 报错（W1 #66）。
func (b *BridgeFS) Setxattr(path string, name string, value []byte, flags int) int {
	if b.trace {
		b.log.Tracef("Setxattr %q %q (accepted, read-only) flags=%d", path, name, flags)
	}
	return 0
}

func (b *BridgeFS) Getxattr(path string, name string) (int, []byte) {
	return -fuse.ENOATTR, nil
}

func (b *BridgeFS) Removexattr(path string, name string) int               { return 0 }
func (b *BridgeFS) Listxattr(path string, fill func(name string) bool) int { return 0 }

// ---------- 句柄表 ----------

func (b *BridgeFS) fileByFH(fh uint64) *openFile {
	if fh == 0 || fh == ^uint64(0) {
		return nil
	}
	b.fhMu.Lock()
	defer b.fhMu.Unlock()
	return b.files[fh]
}

func (b *BridgeFS) fileNodeByFH(fh uint64) *node {
	if f := b.fileByFH(fh); f != nil {
		return f.n
	}
	return nil
}
