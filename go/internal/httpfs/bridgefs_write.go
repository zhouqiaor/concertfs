// bridgefs_write.go M3 写回调实现（rw 模式；ro 由 bridgefs.go 分流拒绝）。
//
// 写模型：
//   - 写句柄持有挂载会话目录中的磁盘临时文件；已有文件首次读/写前用固定
//     1MiB 缓冲 Range 流式加载，不按文件大小分配内存。
//   - Write 记录并合并非对齐脏区间；已有文件 Flush 仅上传本轮脏块，使用
//     X-Base-ETag 乐观并发校验与 X-Upload-Final 显式提交。失败保留全部脏块
//     和 Upload-Id，成功后清空；412 不降级为全量覆盖。
//   - 新文件及零长度提交走完整上传（空文件没有可发送的 delta 块）；
//     已有文件 O_TRUNC 后的非空映像也走 base delta，并显式覆盖零洞：
//     ≤4MiB 完整写 PutContent，>4MiB 旧分块协议续传。
//   - 写后一致性：失效本 fh 读块缓存与父目录 dirCache；Create/Rename/Delete
//     同步维护 byPath 键与父目录缓存。
//   - 目录改名/移动恒 ENOSYS（FR-19，M4）；非零 Truncate 恒 ENOSYS（P2-12）。
package httpfs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/winfsp/cgofuse/fuse"
)

// wholeWriteThreshold ≤4MiB 走整写；以上按 1MiB 块分块上传。
const wholeWriteThreshold = 4 << 20

type dirtyRange struct {
	start int64
	end   int64 // 开区间
}

// initTempSession 只创建本次挂载独占的随机目录；不扫其他会话，避免误删活动写句柄。
func (b *BridgeFS) initTempSession() {
	cache, err := os.UserCacheDir() // Windows: %LOCALAPPDATA%
	if err == nil {
		err = os.MkdirAll(filepath.Join(cache, "PhoneFS", "tmp"), 0o700)
	}
	if err == nil {
		b.tempDir, err = os.MkdirTemp(filepath.Join(cache, "PhoneFS", "tmp"), "mount-")
	}
	if err != nil {
		b.tempErr = err
		b.log.Errorf("write temp session unavailable: %v", err)
	}
}

func (b *BridgeFS) createTempImage(f *openFile) int {
	if b.tempErr != nil {
		b.log.Errorf("create write temp file: %v", b.tempErr)
		return -fuse.EIO
	}
	file, err := os.CreateTemp(b.tempDir, "write-*")
	if err != nil {
		b.log.Errorf("create write temp file in %q: %v", b.tempDir, err)
		return -fuse.EIO
	}
	f.file, f.tempPath = file, file.Name()
	return 0
}

// cleanupTempFile 幂等关闭并删除句柄自己的临时文件；调用方持 f.mu。
func (b *BridgeFS) cleanupTempFile(f *openFile) {
	if f.file != nil {
		if err := f.file.Close(); err != nil {
			b.log.Warnf("close write temp %q: %v", f.tempPath, err)
		}
		f.file = nil
	}
	if f.tempPath != "" {
		if err := os.Remove(f.tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			b.log.Warnf("remove write temp %q: %v", f.tempPath, err)
		}
		f.tempPath = ""
	}
}

func (b *BridgeFS) localIOErrno(op, path string, err error) int {
	b.log.Errorf("%s %s local temp file: %v", op, slashOrRoot(path), err)
	return -fuse.EIO
}

// addDirtyRange 插入 [start,end) 并合并重叠/相邻区间，保持 start 排序。
func addDirtyRange(ranges []dirtyRange, start, end int64) []dirtyRange {
	if end <= start {
		return ranges
	}
	i := sort.Search(len(ranges), func(i int) bool { return ranges[i].end >= start })
	j := i
	for j < len(ranges) && ranges[j].start <= end {
		if ranges[j].start < start {
			start = ranges[j].start
		}
		if ranges[j].end > end {
			end = ranges[j].end
		}
		j++
	}
	if i == j {
		ranges = append(ranges, dirtyRange{})
		copy(ranges[i+1:], ranges[i:])
		ranges[i] = dirtyRange{start: start, end: end}
		return ranges
	}
	ranges[i] = dirtyRange{start: start, end: end}
	copy(ranges[i+1:], ranges[j:])
	return ranges[:len(ranges)-(j-i-1)]
}

func entryBaseETag(size int64, mod time.Time) string {
	var ms int64
	if !mod.IsZero() {
		ms = mod.UnixMilli()
	}
	return fmt.Sprintf("\"%d-%d\"", size, ms)
}

func normalizeBaseETag(etag string) string {
	if etag == "" || strings.HasPrefix(etag, "\"") {
		return etag
	}
	return "\"" + etag + "\""
}

// applyUploadETag 让后续句柄从 node 构造出的 base 与服务端提交值一致。
func applyUploadETag(n *node, etag string, fallbackSize int64) {
	n.size = fallbackSize
	n.mod = time.Now()
	etag = strings.Trim(etag, "\"")
	sizeText, msText, ok := strings.Cut(etag, "-")
	if !ok {
		return
	}
	size, err1 := strconv.ParseInt(sizeText, 10, 64)
	ms, err2 := strconv.ParseInt(msText, 10, 64)
	if err1 == nil && err2 == nil {
		n.size, n.mod = size, time.UnixMilli(ms)
	}
}

// ---------- 句柄/节点辅助 ----------

func (b *BridgeFS) registerFile(f *openFile) uint64 {
	b.fhMu.Lock()
	defer b.fhMu.Unlock()
	b.fhSeq++
	b.files[b.fhSeq] = f
	return b.fhSeq
}

func (b *BridgeFS) setNodeSize(n *node, size int64) {
	b.mu.Lock()
	n.size = size
	n.mod = time.Now()
	b.mu.Unlock()
}

// internalParent "/x/y"→"/x"；"/x"→""（内部根键）。
func internalParent(path string) string {
	i := strings.LastIndex(path, "/")
	if i <= 0 {
		return ""
	}
	return path[:i]
}

func newUploadID() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

// invalidateDirLocked 失效一个目录缓存；调用方持 b.mu。
func (b *BridgeFS) invalidateDirLocked(dirPath string) {
	delete(b.dirs, dirPath)
}

// ---------- Create / Mkdir ----------

// createFile 建普通文件并开写句柄（Create 回调与 Open(O_CREAT) 共用）。
func (b *BridgeFS) createFile(path string, flags int) (int, uint64) {
	n, errc := b.createEntry(path, false)
	if errc != 0 {
		return errc, ^uint64(0)
	}
	f := &openFile{n: n, blocks: map[int64][]byte{}, write: true, loaded: true, fullUpload: true}
	if errc := b.createTempImage(f); errc != 0 {
		return errc, ^uint64(0)
	}
	fh := b.registerFile(f)
	if b.trace {
		b.log.Tracef("createFile %q flags=%#x fh=%d", path, flags, fh)
	}
	return 0, fh
}

func (b *BridgeFS) mkdir(path string) int {
	_, errc := b.createEntry(path, true)
	return errc
}

// createEntry 是 create/mkdir 公共路径：本地名校验 → 父目录解析 →
// POST create → 注册节点 + 失效父目录缓存。
func (b *BridgeFS) createEntry(path string, isDir bool) (*node, int) {
	comps := splitComps(path)
	if len(comps) == 0 {
		return nil, -fuse.EEXIST // 根已存在
	}
	name := comps[len(comps)-1]
	if !legalName(name) {
		// 与服务端 400 INVALID_NAME 同源的前置校验（Windows 名规则 §5.1）
		return nil, -fuse.EINVAL
	}
	parentPath := internalParent(path)
	pn, errc := b.resolve(parentPath)
	if errc != 0 {
		return nil, errc
	}
	if !pn.isDir {
		return nil, -fuse.ENOTDIR
	}
	// 虚拟根与分类根不可写（分类后端 W2 未实现；根布局固定 8 行）
	if pn.id == "" || strings.HasPrefix(pn.id, "cat:") {
		return nil, -fuse.EROFS
	}
	e, err := b.cl.Create(context.Background(), pn.id, name, isDir, "")
	if err != nil {
		return nil, b.apiErrno(err, "create", path)
	}
	b.mu.Lock()
	n := b.upsertLocked(path, &e)
	b.invalidateDirLocked(parentPath)
	b.mu.Unlock()
	if b.trace {
		b.log.Tracef("createEntry %q isDir=%v id=%s", path, isDir, e.ID)
	}
	return n, 0
}

// ---------- Write / Read（写句柄） ----------

func (b *BridgeFS) write(path string, buff []byte, ofst int64, fh uint64) int {
	if ofst < 0 {
		return -fuse.EINVAL
	}
	f := b.fileByFH(fh)
	if f == nil || !f.write {
		return -fuse.EBADF
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if errc := b.ensureLoaded(path, f); errc != 0 {
		return errc
	}
	end := ofst + int64(len(buff))
	if end < ofst {
		return -fuse.EINVAL
	}
	if len(buff) > 0 {
		if _, err := f.file.WriteAt(buff, ofst); err != nil {
			return b.localIOErrno("write", path, err)
		}
		f.dirtyRanges = addDirtyRange(f.dirtyRanges, ofst, end)
		f.dirty = true
		if end > f.size {
			f.size = end
		}
	}
	b.setNodeSize(f.n, f.size)
	if b.trace {
		b.log.Tracef("write %q ofst=%d len=%d size=%d dirtyRanges=%d", path, ofst, len(buff), f.size, len(f.dirtyRanges))
	}
	return len(buff)
}

// readWriteImage 写句柄的 Read：与 Write 共用临时文件保证 O_RDWR 一致。
func (b *BridgeFS) readWriteImage(path string, f *openFile, buff []byte, ofst int64) int {
	if errc := b.ensureLoaded(path, f); errc != 0 {
		return errc
	}
	if ofst >= f.size {
		return 0
	}
	end := ofst + int64(len(buff))
	if end > f.size {
		end = f.size
	}
	n, err := f.file.ReadAt(buff[:end-ofst], ofst)
	if err != nil && !(errors.Is(err, io.EOF) && n > 0) {
		return b.localIOErrno("read", path, err)
	}
	return n
}

// ensureLoaded 非截断写打开已有文件时以固定缓冲流式拉回原文。
func (b *BridgeFS) ensureLoaded(path string, f *openFile) int {
	if f.loaded {
		return 0
	}
	b.mu.Lock()
	size, mod := f.n.size, f.n.mod
	b.mu.Unlock()
	if err := f.file.Truncate(size); err != nil {
		return b.localIOErrno("write-load-size", path, err)
	}
	for start := int64(0); start < size; {
		end := start + blockSize
		if end > size {
			end = size
		}
		res, err := b.cl.ContentRange(context.Background(), f.n.id, start, end-1)
		if err != nil {
			return b.apiErrno(err, "write-load", path)
		}
		if res.First != start || int64(len(res.Data)) != end-start {
			return b.localIOErrno("write-load", path, fmt.Errorf("bad range [%d-%d): first=%d len=%d", start, end, res.First, len(res.Data)))
		}
		// Truncate 已提供全零洞；跳过全零页可避免稀疏大文件写占满磁盘。
		allZero := true
		for _, v := range res.Data {
			if v != 0 {
				allZero = false
				break
			}
		}
		if !allZero {
			if _, err := f.file.WriteAt(res.Data, start); err != nil {
				return b.localIOErrno("write-load", path, err)
			}
		}
		start = end
	}
	f.size = size
	f.baseETag = entryBaseETag(size, mod)
	f.loaded = true
	if b.trace {
		b.log.Tracef("ensureLoaded %q original=%d bytes base=%s", path, size, f.baseETag)
	}
	return 0
}

// ---------- 上传 ----------

func (b *BridgeFS) upload(path string, f *openFile) int {
	if !f.dirty {
		return 0
	}
	if errc := b.ensureLoaded(path, f); errc != 0 {
		return errc
	}
	var etag string
	var err error
	if f.fullUpload || f.size == 0 { // 空文件没有可携带 X-Upload-Final 的非空 delta 块
		etag, err = b.uploadWhole(f)
	} else {
		etag, err = b.uploadDelta(f)
	}
	if err != nil {
		return b.apiErrno(err, "upload", path)
	}
	if etag == "" {
		return b.localIOErrno("upload", path, errors.New("server completed upload without etag"))
	}
	f.dirty = false
	f.dirtyRanges = nil
	f.fullUpload = false
	f.truncated = false
	f.uploadID = ""
	f.baseETag = normalizeBaseETag(etag)
	// 写后失效：本 fh 读块缓存 + 父目录 dirCache（任务书一致性要求）
	f.blocks = map[int64][]byte{}
	f.lru = nil
	b.mu.Lock()
	applyUploadETag(f.n, etag, f.size)
	f.n.version++ // 使同 node 的其他已打开读句柄废弃旧块
	b.invalidateDirLocked(internalParent(path))
	b.mu.Unlock()
	if b.trace {
		b.log.Tracef("upload %q size=%d complete etag=%s", path, f.size, etag)
	}
	return 0
}

func (b *BridgeFS) readTempChunk(f *openFile, start, end int64) ([]byte, error) {
	data := make([]byte, end-start)
	_, err := f.file.ReadAt(data, start)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (b *BridgeFS) uploadWhole(f *openFile) (string, error) {
	if f.size <= wholeWriteThreshold {
		data, err := b.readTempChunk(f, 0, f.size)
		if err != nil {
			return "", err
		}
		return b.cl.PutContent(context.Background(), f.n.id, data, f.setMtime)
	}
	return b.uploadChunked(f)
}

// uploadDelta 只读并发送本轮脏区间；同一失败会话的块可安全重传。
func (b *BridgeFS) uploadDelta(f *openFile) (string, error) {
	if len(f.dirtyRanges) == 0 {
		return "", errors.New("delta upload has no dirty ranges")
	}
	if f.baseETag == "" {
		return "", errors.New("delta upload has no base etag")
	}
	if f.uploadID == "" {
		f.uploadID = newUploadID()
	}
	ranges := f.dirtyRanges
	if f.truncated {
		// setLength(T) 会保留基底中未覆盖的字节；截断映像的零洞也必须显式覆盖。
		ranges = []dirtyRange{{start: 0, end: f.size}}
	}
	for ri, r := range ranges {
		for start := r.start; start < r.end; {
			end := start + blockSize
			if end > r.end {
				end = r.end
			}
			data, err := b.readTempChunk(f, start, end)
			if err != nil {
				return "", err
			}
			final := ri == len(ranges)-1 && end == r.end
			_, complete, etag, err := b.cl.PutChunkWithOptions(context.Background(), f.n.id, start, f.size, data, PutChunkOptions{
				UploadID: f.uploadID, BaseETag: f.baseETag, Final: final, MtimeMs: f.setMtime,
			})
			if err != nil {
				return "", err
			}
			if final {
				if !complete {
					return "", errors.New("final delta chunk acked without complete")
				}
				return etag, nil
			}
			if complete {
				return "", errors.New("non-final delta chunk unexpectedly committed")
			}
			start = end
		}
	}
	return "", errors.New("delta upload ended without final chunk")
}

// uploadChunked 按 1MiB 块从临时文件顺序上传；409 UPLOAD_INCOMPLETE 按
// received 水位续传（覆盖乱序/缺块场景）；同 uploadID 重传幂等。
func (b *BridgeFS) uploadChunked(f *openFile) (string, error) {
	uploadID := newUploadID()
	total := f.size
	start := int64(0)
	resumeRetries := 0
	for start < total {
		end := start + blockSize
		if end > total {
			end = total
		}
		data, err := b.readTempChunk(f, start, end)
		if err != nil {
			return "", err
		}
		received, complete, etag, err := b.cl.PutChunk(context.Background(), f.n.id, uploadID, start, total, data, f.setMtime)
		if err != nil {
			var ae *APIError
			if errors.As(err, &ae) && ae.Code == "UPLOAD_INCOMPLETE" {
				// 服务端告知续传水位：跳到 received 补块（含乱序补全）
				if received >= 0 && received != start && received < total && resumeRetries < 8 {
					resumeRetries++
					b.log.Warnf("upload %q: UPLOAD_INCOMPLETE, resume at %d (was %d)", f.n.name, received, start)
					start = received
					continue
				}
			}
			return "", err
		}
		resumeRetries = 0
		if complete {
			if received != total {
				return "", fmt.Errorf("upload complete but received=%d != total=%d", received, total)
			}
			return etag, nil
		}
		if end == total {
			return "", fmt.Errorf("last chunk acked without complete (received=%d total=%d)", received, total)
		}
		// 非末块：以服务端水位为准前进（正常 received==end）
		if received > start {
			start = received
		} else {
			start = end
		}
	}
	return "", fmt.Errorf("upload ended without complete (total=%d)", total)
}

// ---------- Truncate（截零支持；非零 P2-12 ENOSYS） ----------

func (b *BridgeFS) truncate(path string, size int64, fh uint64) int {
	if size != 0 {
		// SPEC P2-12 冻结：非零长度缩短/扩展不支持，PATCH meta 无 size 字段
		return -fuse.ENOSYS
	}
	if f := b.fileByFH(fh); f != nil && f.write {
		f.mu.Lock()
		defer f.mu.Unlock()
		if err := f.file.Truncate(0); err != nil {
			return b.localIOErrno("truncate0", path, err)
		}
		if !f.fullUpload && f.baseETag == "" {
			b.mu.Lock()
			f.baseETag = entryBaseETag(f.n.size, f.n.mod)
			b.mu.Unlock()
		}
		f.size, f.loaded, f.dirty = 0, true, true
		f.truncated = true
		f.dirtyRanges = nil
		f.uploadID = ""
		b.setNodeSize(f.n, 0)
		return 0
	}
	// 无写句柄截零：openWrite(truncate) 空写关闭 = 整写空内容（SPEC §5.10）
	n, errc := b.resolve(path)
	if errc != 0 {
		return errc
	}
	if isVirtualAutorun(n) {
		return -fuse.EROFS
	}
	if n.isDir {
		return -fuse.EISDIR
	}
	if _, err := b.cl.PutContent(context.Background(), n.id, []byte{}, 0); err != nil {
		return b.apiErrno(err, "truncate0", path)
	}
	b.mu.Lock()
	n.size = 0
	n.mod = time.Now()
	n.version++
	b.invalidateDirLocked(internalParent(path))
	b.mu.Unlock()
	return 0
}

// ---------- Delete / Rename / Utimens ----------

func (b *BridgeFS) unlink(path string) int {
	return b.deleteEntry(path, false)
}

func (b *BridgeFS) rmdir(path string) int {
	return b.deleteEntry(path, true)
}

// deleteEntry 删文件/目录：404 幂等成功；409 NOT_EMPTY→ENOTEMPTY；
// 409 PARTIAL→EIO（残留告警，details.deleted/failed 留痕）。
func (b *BridgeFS) deleteEntry(path string, wantDir bool) int {
	n, errc := b.resolve(path)
	if errc != 0 {
		if errc == -fuse.ENOENT {
			return 0 // 幂等：目标本就不存在
		}
		return errc
	}
	if isVirtualAutorun(n) {
		return -fuse.EROFS
	}
	if wantDir && !n.isDir {
		return -fuse.ENOTDIR
	}
	if !wantDir && n.isDir {
		return -fuse.EISDIR
	}
	err := b.cl.Delete(context.Background(), n.id, false)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) {
			switch {
			case ae.Code == "NOT_EMPTY":
				return -fuse.ENOTEMPTY
			case ae.Code == "PARTIAL":
				b.log.Errorf("delete %q PARTIAL: details=%v (residue on device)", path, ae.Details)
				return -fuse.EIO
			case ae.HTTPStatus == 404:
				// 幂等成功，落到本地清理
			default:
				return b.apiErrno(err, "delete", path)
			}
		} else {
			return b.apiErrno(err, "delete", path)
		}
	}
	b.mu.Lock()
	delete(b.byPath, path)
	b.invalidateDirLocked(internalParent(path))
	b.mu.Unlock()
	if b.trace {
		b.log.Tracef("deleteEntry %q wantDir=%v done", path, wantDir)
	}
	return 0
}

// rename 仅文件改名+移动（FR-19：目录源恒 ENOSYS，M4）。
// 目标已存在时发 replace=true，由服务端单次 PATCH 原子替换，不预 DELETE。
func (b *BridgeFS) rename(oldpath string, newpath string) int {
	n, errc := b.resolve(oldpath)
	if errc != 0 {
		return errc
	}
	oldCanon := b.canonicalPath(oldpath)
	if isVirtualAutorun(n) {
		return -fuse.EROFS
	}
	if n.isDir {
		return -fuse.ENOSYS // FR-19：目录改名/移动 M4（服务端同口径 501）
	}
	comps := splitComps(newpath)
	if len(comps) == 0 {
		return -fuse.EINVAL
	}
	newName := comps[len(comps)-1]
	if !legalName(newName) {
		return -fuse.EINVAL
	}
	newParent := internalParent(newpath)
	pn, errc := b.resolve(newParent)
	if errc != 0 {
		return errc
	}
	newParentCanon := b.canonicalPath(newParent)
	if !pn.isDir {
		return -fuse.ENOTDIR
	}
	if pn.id == "" || strings.HasPrefix(pn.id, "cat:") {
		return -fuse.EROFS // 虚拟根/分类根不可作为移动目标
	}
	replace := false
	targetCanon := ""
	if tn, terrc := b.resolve(newpath); terrc == 0 && tn != nil {
		if isVirtualAutorun(tn) {
			return -fuse.EROFS
		}
		if tn.isDir {
			return -fuse.EISDIR
		}
		if tn.id != n.id {
			replace = true
			targetCanon = b.canonicalPath(newpath)
		}
	}
	e, err := b.cl.PatchMeta(context.Background(), n.id, &newName, &pn.id, nil, replace)
	if err != nil {
		return b.apiErrno(err, "rename", oldpath)
	}
	newCanon := newParentCanon + "/" + e.Name
	b.mu.Lock()
	// 事务性迁移同一个 node：旧键删除、新键建立，已打开句柄/版本号/ino
	// 全部随节点保留。不要 upsert 出新节点，否则句柄指向旧对象。
	// byPath/dirs 一律使用规范路径：WinFsp 可能传入大写父目录别名，直接保存
	// newpath 会让目标 dirCache 未失效，紧随其后的 Utimens 读到空目录（harness 10）。
	delete(b.byPath, oldpath)
	delete(b.byPath, oldCanon)
	if replace {
		delete(b.byPath, newpath)
		delete(b.byPath, targetCanon)
	}
	n.id, n.name, n.isDir, n.size, n.writable = e.ID, e.Name, e.IsDir, e.Size, e.Writable
	if e.ModifiedMs > 0 {
		n.mod = time.UnixMilli(e.ModifiedMs)
	}
	b.byPath[newCanon] = n
	b.invalidateDirLocked(internalParent(oldpath))
	b.invalidateDirLocked(internalParent(oldCanon))
	b.invalidateDirLocked(newParent)
	b.invalidateDirLocked(newParentCanon)
	b.mu.Unlock()
	if b.trace {
		b.log.Tracef("rename %q -> %q done", oldpath, newpath)
	}
	return 0
}

// utimens 文件走 PATCH meta modified_ms；目录与 501 降级空操作成功
// （Windows 复制/创建后常带 SetFileTime，时间戳失败不应让操作报错；
//
//	写句柄的 mtime 另经 X-Set-Mtime 随上传生效）。
func (b *BridgeFS) utimens(path string, tmsp []fuse.Timespec) int {
	n, errc := b.resolve(path)
	if errc != 0 {
		return errc
	}
	if isVirtualAutorun(n) {
		return -fuse.EROFS
	}
	var ms int64
	if tmsp == nil {
		ms = time.Now().UnixMilli()
	} else {
		if len(tmsp) < 2 {
			return -fuse.EINVAL
		}
		ms = tmsp[1].Sec*1000 + tmsp[1].Nsec/1e6 // [0]=atime [1]=mtime
	}
	if !n.isDir {
		if _, err := b.cl.PatchMeta(context.Background(), n.id, nil, nil, &ms, false); err != nil {
			var ae *APIError
			if errors.As(err, &ae) && ae.HTTPStatus == 501 {
				b.log.Warnf("utimens %q: server 501, no-op fallback", path)
			} else {
				return b.apiErrno(err, "utimens", path)
			}
		}
	}
	b.mu.Lock()
	n.mod = time.UnixMilli(ms)
	b.mu.Unlock()
	return 0
}
