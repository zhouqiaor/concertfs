// bridgefs.go 把 httpfs.Client 适配为 cgofuse 只读文件系统（M1：Getattr/
// Readdir/Open/Read/Statfs + Windows 兼容层）。
//
// 路径模型：FUSE 路径 "/内部存储/PhoneFS-w2/test10mb.bin"。盘符根是虚拟目录，
// 其 8 个子项直接铺服务端 /v1/roots；分类根（cat:*）在 W2 列表返回 501，
// 按空目录呈现（只记一次日志）。
//
// 读路径：文件按 1MiB 对齐块做 Range 请求，每打开句柄最多缓存 16 块（16MiB）
// LRU，绝不整文件下载；大小来自 list 的 size 属性，越界读返回 0（EOF）。
package httpfs

import (
	"context"
	"errors"
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
}

type dirCache struct {
	entries []*node
	at      time.Time
}

type openFile struct {
	n      *node
	mu     sync.Mutex // 串行化同句柄块取/缓存与写缓冲（WinFsp 可能并发下发）
	blocks map[int64][]byte
	lru    []int64

	// M2 写句柄状态：write=true 时读写都走 data 内存映像（M2 限制：
	// 写缓冲=整文件内存，大文件写 M3 评估落盘；上传阶段按 1MiB 分块）。
	write    bool
	dirty    bool
	data     []byte
	loaded   bool  // data 是否已含完整内容（新文件/O_TRUNC 打开即 true）
	setMtime int64 // Utimens 记录的 mtime(ms)，上传时 X-Set-Mtime
}

// BridgeFS 实现 fuse.FileSystemInterface 的桥（M1 只读；M2 -ro=false 时读写）。
type BridgeFS struct {
	fuse.FileSystemBase // 未实现回调默认 -ENOSYS（Setxattr 等见下覆写）

	cl       *Client
	log      Logger
	trace    bool
	readOnly bool // -ro 开关：true 时维持 M1 全部 EROFS/Access 只读行为

	mountTime   time.Time
	root        *node
	autorun     *node
	autorunData []byte

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

// NewBridgeFS 创建桥文件系统；随后必须 Warmup 再挂载。
// readOnly=true 对应 -ro 开关（M1 行为）；false 开放 M2 写回调。
func NewBridgeFS(cl *Client, log Logger, trace bool, readOnly bool) *BridgeFS {
	b := &BridgeFS{
		cl:        cl,
		log:       log,
		trace:     trace,
		readOnly:  readOnly,
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
	rootEntries := b.storeEntriesLocked("", roots)
	rootEntries = b.appendVirtualRootLocked("", rootEntries)
	b.dirs[""] = &dirCache{entries: rootEntries, at: time.Now()}
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
	nodes = b.appendVirtualRootLocked(dirPath, nodes)
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
		n.id, n.isDir, n.size, n.mod, n.writable = e.ID, e.IsDir, e.Size, mod, e.Writable
		n.name = e.Name
		return n
	}
	b.inoSeq++
	n := &node{
		id: e.ID, name: e.Name, isDir: e.IsDir, size: e.Size,
		mod: mod, writable: e.Writable, ino: b.inoSeq,
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

// apiErrno 记录并映射服务端错误：401/403→EACCES、404→ENOENT、
// 409 ALREADY_EXISTS→EEXIST、409 NOT_EMPTY→ENOTEMPTY、400 INVALID_NAME→EINVAL、
// 507→ENOSPC、501（非 cat 分支）→ENOSYS、416 由 Read 特判、其余→EIO。
func (b *BridgeFS) apiErrno(err error, op, path string) int {
	var ae *APIError
	if errors.As(err, &ae) {
		switch ae.HTTPStatus {
		case 401, 403:
			b.log.Warnf("%s %s: auth/scope failure: %v", op, slashOrRoot(path), ae)
			return -fuse.EACCES
		case 404:
			if b.trace {
				b.log.Tracef("%s %s: not found: %v", op, slashOrRoot(path), ae.Code)
			}
			return -fuse.ENOENT
		case 400:
			if ae.Code == "INVALID_NAME" {
				b.log.Warnf("%s %s: invalid name: %v", op, slashOrRoot(path), ae)
				return -fuse.EINVAL
			}
		case 409:
			switch ae.Code {
			case "ALREADY_EXISTS":
				return -fuse.EEXIST
			case "NOT_EMPTY":
				return -fuse.ENOTEMPTY
			case "PARTIAL":
				b.log.Errorf("%s %s: PARTIAL details=%v", op, slashOrRoot(path), ae.Details)
				return -fuse.EIO
			}
		case 416:
			return 0 // Read 路径特判；其它路径理论不可达
		case 501:
			b.log.Warnf("%s %s: not implemented: %v", op, slashOrRoot(path), ae)
			return -fuse.ENOSYS
		case 507:
			b.log.Warnf("%s %s: quota exhausted: %v", op, slashOrRoot(path), ae)
			return -fuse.ENOSPC
		}
	}
	b.log.Errorf("%s %s failed: %v", op, slashOrRoot(path), err)
	return -fuse.EIO
}

// ---------- FUSE 生命周期 / 容量 ----------

// Init 由 cgofuse 在挂载线程启动时回调。
func (b *BridgeFS) Init() {
	b.log.Infof("Init: phonefs-m1 read-only bridge ready (block=%dMiB cache=%d/fh)",
		blockSize/(1<<20), maxBlocksPerFH)
}

func (b *BridgeFS) Destroy() {
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
		st.Mode = fuse.S_IFDIR | 0o555
		st.Nlink = 2
		return
	}
	st.Mode = fuse.S_IFREG | 0o444
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
	if n := b.fileNodeByFH(fh); n != nil {
		b.fillAttr(n, st)
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
	wantWrite := flags&fuse.O_ACCMODE != fuse.O_RDONLY || flags&(fuse.O_CREAT|fuse.O_TRUNC) != 0
	n, errc := b.resolve(path)
	// rw 模式 O_CREAT：节点不存在则就地创建（Windows CreateFile OPEN_ALWAYS 路径）
	if errc == -fuse.ENOENT && wantWrite && !b.readOnly && flags&fuse.O_CREAT != 0 {
		return b.createFile(path, flags)
	}
	if errc != 0 {
		return errc, ^uint64(0)
	}
	if n.isDir {
		return -fuse.EISDIR, ^uint64(0)
	}
	if wantWrite && isVirtualAutorun(n) {
		return -fuse.EROFS, ^uint64(0) // 虚拟图标配置永远只读，rw 挂载也不例外
	}
	if wantWrite && b.readOnly {
		// M1 防线：ro 挂载下内核应已挡掉写打开；到这里也给明确写保护。
		b.log.Warnf("write-open refused on read-only bridge: %q flags=%#x", path, flags)
		return -fuse.EROFS, ^uint64(0)
	}
	f := &openFile{n: n, blocks: map[int64][]byte{}}
	if wantWrite {
		f.write = true
		if flags&fuse.O_TRUNC != 0 {
			f.data, f.loaded = []byte{}, true
			if n.size > 0 {
				f.dirty = true
			}
			b.setNodeSize(n, 0)
		}
	}
	fh := b.registerFile(f)
	if b.trace {
		b.log.Tracef("Open %q size=%d flags=%#x write=%v fh=%d", path, n.size, flags, f.write, fh)
	}
	return 0, fh
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

	if isVirtualAutorun(f.n) {
		return b.readVirtualAutorun(buff, ofst)
	}

	// M2 写句柄：读写同走 data 内存映像（O_RDWR 一致性；ensureLoaded 可能拉网）
	if f.write {
		return b.readWriteImage(path, f, buff, ofst)
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
		return 0 // WinFsp 在只读打开/目录等路径也会 Flush；无句柄不算错
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.write || !f.dirty {
		return 0
	}
	return b.upload(path, f)
}

func (b *BridgeFS) Fsync(path string, datasync bool, fh uint64) int {
	return b.Flush(path, fh) // M2：Fsync 与 Flush 同语义（上传即服务端原子替换）
}

func (b *BridgeFS) Release(path string, fh uint64) int {
	b.fhMu.Lock()
	f := b.files[fh]
	b.fhMu.Unlock()
	if f == nil {
		return -fuse.EBADF
	}
	errc := 0
	f.mu.Lock()
	if f.write && f.dirty {
		errc = b.upload(path, f) // 关闭前兜底上传（Explorer 复制依赖 Release 落盘）
	}
	f.mu.Unlock()
	b.fhMu.Lock()
	delete(b.files, fh)
	b.fhMu.Unlock()
	if b.trace {
		b.log.Tracef("Release %q fh=%d cachedBlocks=%d write=%v errc=%d", path, fh, len(f.blocks), f.write, errc)
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

// ---------- 只读访问与 Windows 兼容层 ----------

func (b *BridgeFS) Access(path string, mask uint32) int {
	// -ro：写（W_OK）与删（DELETE_OK，配合 SetCapDeleteAccess）一律拒绝。
	// 没有这道闸门时 WinFsp 删除走 delete-on-cleanup：DeleteFile 先报成功、
	// Unlink 在句柄清理时才被调用，错误传不回调用方，文件又随目录刷新「复活」。
	// rw：写/删访问放行，由 Unlink/Rmdir/写回调实际执行并如实报错。
	if b.readOnly && mask&(fuse.W_OK|fuse.DELETE_OK) != 0 {
		if b.trace {
			b.log.Tracef("Access %q mask=%#x -> EACCES (read-only)", path, mask)
		}
		return -fuse.EACCES
	}
	n, errc := b.resolve(path)
	if errc != 0 {
		return errc
	}
	if isVirtualAutorun(n) && mask&(fuse.W_OK|fuse.DELETE_OK) != 0 {
		return -fuse.EACCES
	}
	return 0
}

// ---------- 写操作：-ro 维持 M1 EROFS；rw 走 bridgefs_write.go 真实实现 ----------

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
	return b.unlink(path)
}

func (b *BridgeFS) Rmdir(path string) int {
	if b.readOnly {
		return b.roRefused("Rmdir", path)
	}
	return b.rmdir(path)
}

func (b *BridgeFS) Mkdir(path string, mode uint32) int {
	if b.readOnly {
		return b.roRefused("Mkdir", path)
	}
	return b.mkdir(path)
}

func (b *BridgeFS) Mknod(path string, mode uint32, dev uint64) int {
	if b.readOnly {
		return b.roRefused("Mknod", path)
	}
	// Windows 侧 Mknod 即建普通文件（无设备节点语义），与 Create 同路
	errc, _ := b.createFile(path, fuse.O_WRONLY)
	return errc
}

func (b *BridgeFS) Create(path string, flags int, mode uint32) (int, uint64) {
	if b.readOnly {
		return b.roRefused("Create", path), ^uint64(0)
	}
	return b.createFile(path, flags)
}

func (b *BridgeFS) Write(path string, buff []byte, ofst int64, fh uint64) int {
	if b.readOnly {
		return b.roRefused("Write", path)
	}
	return b.write(path, buff, ofst, fh)
}

func (b *BridgeFS) Truncate(path string, size int64, fh uint64) int {
	if b.readOnly {
		return b.roRefused("Truncate", path)
	}
	return b.truncate(path, size, fh)
}

func (b *BridgeFS) Rename(oldpath string, newpath string) int {
	if b.readOnly {
		return b.roRefused("Rename", oldpath)
	}
	return b.rename(oldpath, newpath)
}

// Link/Symlink 恒 ENOSYS（SPEC §5.10 冻结：不实现，对齐华为 xattr 语义）。
func (b *BridgeFS) Link(path string, newpath string) int      { return -fuse.ENOSYS }
func (b *BridgeFS) Symlink(target string, newpath string) int { return -fuse.ENOSYS }

// Chmod/Chown：rw 时 FAT 语义空操作成功（SPEC §5.10）；ro 时写保护。
func (b *BridgeFS) Chmod(path string, mode uint32) int {
	if b.readOnly {
		return b.roRefused("Chmod", path)
	}
	return 0
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
	return b.utimens(path, tmsp)
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
