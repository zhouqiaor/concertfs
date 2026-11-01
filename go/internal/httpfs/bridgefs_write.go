// bridgefs_write.go M2 写回调实现（rw 模式；ro 由 bridgefs.go 分流拒绝）。
//
// 写模型（M2 冻结，YAGNI）：
//   - 写句柄持有整文件内存映像 data（新文件/O_TRUNC 从空开始；非截断写打开
//     已有文件时首次读/写前 ensureLoaded 拉回原文）。内存=文件大小，
//     大文件写落盘排 M3；上传阶段按 1MiB 分块，不受内存映像影响协议正确性。
//   - Write 只改映像并标脏；Flush/Fsync/Release 触发上传：
//     ≤4MiB 整写 PutContent（原子替换），>4MiB 分块 PutChunk（X-Upload-Id
//     幂等续传，409 UPLOAD_INCOMPLETE 按 received 水位续传/乱序补全）。
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
	"strings"
	"time"

	"github.com/winfsp/cgofuse/fuse"
)

// wholeWriteThreshold ≤4MiB 走整写；以上按 1MiB 块分块上传。
const wholeWriteThreshold = 4 << 20

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
	f := &openFile{n: n, blocks: map[int64][]byte{}, write: true, data: []byte{}, loaded: true}
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
	if end > int64(len(f.data)) {
		f.data = append(f.data, make([]byte, end-int64(len(f.data)))...)
	}
	copy(f.data[ofst:end], buff)
	f.dirty = true
	b.setNodeSize(f.n, int64(len(f.data)))
	if b.trace {
		b.log.Tracef("write %q ofst=%d len=%d size=%d", path, ofst, len(buff), len(f.data))
	}
	return len(buff)
}

// readWriteImage 写句柄的 Read：与 Write 共用内存映像保证 O_RDWR 一致。
func (b *BridgeFS) readWriteImage(path string, f *openFile, buff []byte, ofst int64) int {
	if errc := b.ensureLoaded(path, f); errc != 0 {
		return errc
	}
	if ofst >= int64(len(f.data)) {
		return 0
	}
	end := ofst + int64(len(buff))
	if end > int64(len(f.data)) {
		end = int64(len(f.data))
	}
	return copy(buff, f.data[ofst:end])
}

// ensureLoaded 非截断写打开已有文件时拉回原文（分块 Range，不整包一次读）。
func (b *BridgeFS) ensureLoaded(path string, f *openFile) int {
	if f.loaded {
		return 0
	}
	b.mu.Lock()
	size := f.n.size
	b.mu.Unlock()
	data := make([]byte, 0, size)
	for start := int64(0); start < size; start += blockSize {
		end := start + blockSize - 1
		if end >= size {
			end = size - 1
		}
		res, err := b.cl.ContentRange(context.Background(), f.n.id, start, end)
		if err != nil {
			return b.apiErrno(err, "write-load", path)
		}
		data = append(data, res.Data...)
	}
	f.data = data
	f.loaded = true
	if b.trace {
		b.log.Tracef("ensureLoaded %q original=%d bytes", path, len(data))
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
	data := f.data
	var err error
	if int64(len(data)) <= wholeWriteThreshold {
		_, err = b.cl.PutContent(context.Background(), f.n.id, data, f.setMtime)
	} else {
		err = b.uploadChunked(f, data)
	}
	if err != nil {
		return b.apiErrno(err, "upload", path)
	}
	f.dirty = false
	// 写后失效：本 fh 读块缓存 + 父目录 dirCache（任务书一致性要求）
	f.blocks = map[int64][]byte{}
	f.lru = nil
	b.mu.Lock()
	f.n.size = int64(len(data))
	f.n.mod = time.Now()
	f.n.version++ // 使同 node 的其他已打开读句柄废弃旧块
	b.invalidateDirLocked(internalParent(path))
	b.mu.Unlock()
	if b.trace {
		b.log.Tracef("upload %q %d bytes complete", path, len(data))
	}
	return 0
}

// uploadChunked 按 1MiB 块顺序上传；409 UPLOAD_INCOMPLETE 按 received 水位
// 续传（覆盖乱序/缺块场景）；同 uploadID 重传幂等。
func (b *BridgeFS) uploadChunked(f *openFile, data []byte) error {
	uploadID := newUploadID()
	total := int64(len(data))
	start := int64(0)
	resumeRetries := 0
	for start < total {
		end := start + blockSize
		if end > total {
			end = total
		}
		received, complete, _, err := b.cl.PutChunk(context.Background(), f.n.id, uploadID, start, total, data[start:end], f.setMtime)
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
			return err
		}
		resumeRetries = 0
		if complete {
			if received != total {
				return fmt.Errorf("upload complete but received=%d != total=%d", received, total)
			}
			return nil
		}
		if end == total {
			return fmt.Errorf("last chunk acked without complete (received=%d total=%d)", received, total)
		}
		// 非末块：以服务端水位为准前进（正常 received==end）
		if received > start {
			start = received
		} else {
			start = end
		}
	}
	return fmt.Errorf("upload ended without complete (total=%d)", total)
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
		f.data, f.loaded, f.dirty = []byte{}, true, true
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
// 目标已存在按 POSIX 文件替换：先删目标文件再 PATCH meta。
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
	// 目标已存在：文件→先删（替换语义）；目录→EISDIR
	if tn, terrc := b.resolve(newpath); terrc == 0 && tn != nil {
		if isVirtualAutorun(tn) {
			return -fuse.EROFS
		}
		if tn.isDir {
			return -fuse.EISDIR
		}
		if tn.id != n.id {
			if err := b.cl.Delete(context.Background(), tn.id, false); err != nil {
				var ae *APIError
				if !(errors.As(err, &ae) && ae.HTTPStatus == 404) {
					return b.apiErrno(err, "rename-target", newpath)
				}
			}
			targetCanon := b.canonicalPath(newpath)
			b.mu.Lock()
			delete(b.byPath, newpath)
			delete(b.byPath, targetCanon)
			b.mu.Unlock()
		}
	}
	e, err := b.cl.PatchMeta(context.Background(), n.id, &newName, &pn.id, nil)
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
		if _, err := b.cl.PatchMeta(context.Background(), n.id, nil, nil, &ms); err != nil {
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
