// write_test.go M2 写操作桥侧单测：httptest 假服务端（内存文件树，完整实现
// SPEC §6.5.6–6.5.9 契约），驱动真实 httpfs.Client + BridgeFS 公开回调。
// 覆盖：写-读回、分块乱序/缺块续传、Upload-Id 幂等重传、409 ALREADY_EXISTS/
// NOT_EMPTY/PARTIAL、目录 501、大文件分块、写后 dirCache 失效、-ro 拒绝。
// 运行：CGO_ENABLED=0 go test ./test/m2/ -v
package m2_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/winfsp/cgofuse/fuse"

	"phonefs/go/internal/httpfs"
)

// ---------- 假服务端 ----------

type fnode struct {
	id, parent, name string
	isDir            bool
	modMs            int64
	data             []byte
	logicalSize      int64 // >0 时由 Range 动态生成全零稀疏内容
}

func (n *fnode) size() int64 {
	if n.logicalSize > 0 {
		return n.logicalSize
	}
	return int64(len(n.data))
}

func (n *fnode) etag() string {
	return fmt.Sprintf("%d-%d", n.size(), n.modMs)
}

type uploadSession struct {
	chunks  map[int64][]byte // start → 块数据（同 start 重传覆盖，幂等）
	total   int64
	prefix  int64 // 连续前缀水位（旧全覆盖协议的 received 语义）
	target  string
	started bool
	delta   bool
	base    string
	data    []byte
	done    bool
	etag    string
}

type deltaRecord struct {
	uid        string
	start, end int64
	total      int64
	base       string
	final      bool
}

type fakeServer struct {
	mu       sync.Mutex
	nodes    map[string]*fnode
	seq      int
	uploads  map[string]*uploadSession
	listCnt  int
	chunkLog []string // "uploadID start-end/total" 按到达顺序
	deltaLog []deltaRecord
	opLog    []string
	// 故障注入
	incompleteOnce    bool // 首个分块：收下首块但回 409 UPLOAD_INCOMPLETE
	retry503Once      bool // 首个分块：第一次回 503 retriable（不记块）
	deltaFinal503Once bool // delta final 提交后首次回 503，测完成态同 UID 重传
	injected          bool
	deltaInjected     bool
}

func newFakeServer() *fakeServer {
	fs := &fakeServer{
		nodes:   map[string]*fnode{},
		uploads: map[string]*uploadSession{},
	}
	fs.nodes["stor:primary"] = &fnode{id: "stor:primary", name: "内部存储", isDir: true}
	return fs
}

func (fs *fakeServer) child(parent, name string) *fnode {
	for _, n := range fs.nodes {
		if n.parent == parent && n.name == name {
			return n
		}
	}
	return nil
}

func (fs *fakeServer) attr(n *fnode) map[string]any {
	return map[string]any{
		"id": n.id, "parent": n.parent, "name": n.name, "is_dir": n.isDir,
		"size": n.size(), "modified_ms": n.modMs, "writable": true,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string, retriable bool, details map[string]any) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"code": code, "message": msg, "trace_id": "test", "retriable": retriable, "details": details,
	}})
}

func (fs *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	path := r.URL.Path
	id := r.URL.Query().Get("id")
	switch {
	case path == "/v1/pair/info":
		writeJSON(w, 200, map[string]any{"service": "phonefs", "fingerprint": "test",
			"device": map[string]any{"name": "FAKE", "type": "android", "form_factor": "phone"}})
	case path == "/v1/roots":
		writeJSON(w, 200, map[string]any{"roots": []any{fs.attr(fs.nodes["stor:primary"])}})
	case path == "/v1/fs/statfs":
		writeJSON(w, 200, map[string]any{"total_bytes": 1 << 30, "free_bytes": 1 << 29, "block_size": 4096})
	case path == "/v1/fs/list" && r.Method == http.MethodGet:
		fs.listCnt++
		n := fs.nodes[id]
		if n == nil || !n.isDir {
			writeErr(w, 404, "NOT_FOUND", "not found", false, nil)
			return
		}
		entries := []any{}
		for _, ch := range fs.nodes {
			if ch.parent == id {
				entries = append(entries, fs.attr(ch))
			}
		}
		writeJSON(w, 200, map[string]any{"entries": entries, "next_cursor": "", "revision": 1})
	case path == "/v1/fs/content" && r.Method == http.MethodGet:
		n := fs.nodes[id]
		if n == nil || n.isDir {
			writeErr(w, 404, "NOT_FOUND", "not found", false, nil)
			return
		}
		rng := r.Header.Get("Range")
		var start, end int64
		if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &start, &end); err != nil {
			start, end = 0, int64(len(n.data))-1
		}
		if start >= n.size() {
			writeErr(w, 416, "RANGE_NOT_SATISFIABLE", "oob", false, nil)
			return
		}
		if end >= n.size() {
			end = n.size() - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, n.size()))
		w.WriteHeader(206)
		if n.logicalSize > 0 {
			_, _ = w.Write(make([]byte, end-start+1))
		} else {
			_, _ = w.Write(n.data[start : end+1])
		}
	case path == "/v1/fs/create" && r.Method == http.MethodPost:
		var req struct {
			Parent string `json:"parent"`
			Name   string `json:"name"`
			IsDir  bool   `json:"is_dir"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if fs.nodes[req.Parent] == nil {
			writeErr(w, 404, "NOT_FOUND", "parent missing", false, nil)
			return
		}
		if fs.child(req.Parent, req.Name) != nil {
			writeErr(w, 409, "ALREADY_EXISTS", "exists", false, nil)
			return
		}
		fs.seq++
		n := &fnode{id: fmt.Sprintf("p:%d", fs.seq), parent: req.Parent, name: req.Name, isDir: req.IsDir, modMs: 1000}
		fs.nodes[n.id] = n
		writeJSON(w, 201, fs.attr(n))
	case path == "/v1/fs/content" && r.Method == http.MethodPut:
		fs.handlePut(w, r, id)
	case path == "/v1/fs/meta" && r.Method == http.MethodPatch:
		var req struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			ParentID   string `json:"parent_id"`
			Replace    bool   `json:"replace"`
			ModifiedMs *int64 `json:"modified_ms"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		fs.opLog = append(fs.opLog, fmt.Sprintf("patch replace=%v", req.Replace))
		n := fs.nodes[req.ID]
		if n == nil {
			writeErr(w, 404, "NOT_FOUND", "not found", false, nil)
			return
		}
		if req.ModifiedMs != nil {
			// 与真实服务端一致：改 mtime 会改变 ETag(<size>-<mtime>)，从而打断
			// 早先捕获的 delta 基线——这正是 P0-1 回归测试要覆盖的语义。
			n.modMs = *req.ModifiedMs
			writeJSON(w, 200, fs.attr(n))
			return
		}
		if n.isDir {
			writeErr(w, 501, "NOT_SUPPORTED", "dir rename/move M4", false, nil)
			return
		}
		newName, newParent := n.name, n.parent
		if req.Name != "" {
			newName = req.Name
		}
		if req.ParentID != "" {
			newParent = req.ParentID
		}
		if target := fs.child(newParent, newName); target != nil && target.id != n.id {
			if !req.Replace || target.isDir {
				writeErr(w, 409, "ALREADY_EXISTS", "target exists", false, nil)
				return
			}
			delete(fs.nodes, target.id) // 与源节点改名在同一锁内原子完成
		}
		n.name, n.parent = newName, newParent
		writeJSON(w, 200, fs.attr(n))
	case path == "/v1/fs" && r.Method == http.MethodDelete:
		fs.opLog = append(fs.opLog, "delete "+id)
		n := fs.nodes[id]
		if n == nil {
			w.WriteHeader(204) // 幂等
			return
		}
		if strings.HasPrefix(n.name, "partial") {
			writeErr(w, 409, "PARTIAL", "partial delete", false, map[string]any{"deleted": 1, "failed": 2})
			return
		}
		if n.isDir {
			for _, ch := range fs.nodes {
				if ch.parent == id {
					writeErr(w, 409, "NOT_EMPTY", "not empty", false, nil)
					return
				}
			}
		}
		delete(fs.nodes, id)
		w.WriteHeader(204)
	default:
		writeErr(w, 404, "NOT_FOUND", "no route "+path, false, nil)
	}
}

func (fs *fakeServer) handlePut(w http.ResponseWriter, r *http.Request, id string) {
	body, _ := io.ReadAll(r.Body)
	n := fs.nodes[id]
	if n == nil {
		writeErr(w, 404, "NOT_FOUND", "not found", false, nil)
		return
	}
	cr := r.Header.Get("Content-Range")
	if cr == "" { // 整写原子替换
		n.data, n.logicalSize = body, 0
		writeJSON(w, 200, map[string]any{"received": len(body), "etag": fmt.Sprintf("%d-1000", len(body))})
		return
	}
	// 分块：Content-Range: bytes S-E/T + X-Upload-Id
	var start, end, total int64
	_, _ = fmt.Sscanf(cr, "bytes %d-%d/%d", &start, &end, &total)
	uid := r.Header.Get("X-Upload-Id")
	fs.chunkLog = append(fs.chunkLog, fmt.Sprintf("%s %d-%d/%d", uid, start, end, total))
	base := r.Header.Get("X-Base-ETag")
	final := r.Header.Get("X-Upload-Final") == "true"
	if base != "" {
		fs.deltaLog = append(fs.deltaLog, deltaRecord{uid: uid, start: start, end: end, total: total, base: base, final: final})
		fs.handleDeltaPut(w, r, n, uid, base, start, total, body, final)
		return
	}
	// 故障注入 1：503 retriable（不记块），测幂等重传
	if fs.retry503Once && !fs.injected {
		fs.injected = true
		writeErr(w, 503, "TEMPORARY", "busy", true, nil)
		return
	}
	up := fs.uploads[uid]
	if up == nil {
		up = &uploadSession{chunks: map[int64][]byte{}, total: total, target: id}
		fs.uploads[uid] = up
	}
	up.chunks[start] = body
	// 重算连续前缀
	prefix := int64(0)
	for {
		if c, ok := up.chunks[prefix]; ok {
			prefix += int64(len(c))
		} else {
			break
		}
	}
	up.prefix = prefix
	// 故障注入 2：首块已收但回 409 UPLOAD_INCOMPLETE，测 received 续传
	if fs.incompleteOnce && !fs.injected {
		fs.injected = true
		writeErr(w, 409, "UPLOAD_INCOMPLETE", "resume", false, map[string]any{"received": prefix})
		return
	}
	if start+int64(len(body)) == total { // 末块
		if prefix != total {
			writeErr(w, 409, "UPLOAD_INCOMPLETE", "gap", false, map[string]any{"received": prefix})
			return
		}
		full := make([]byte, 0, total)
		for off := int64(0); off < total; {
			c := up.chunks[off]
			full = append(full, c...)
			off += int64(len(c))
		}
		n.data, n.logicalSize = full, 0
		delete(fs.uploads, uid)
		writeJSON(w, 200, map[string]any{"received": total, "complete": true, "etag": fmt.Sprintf("%d-1000", total)})
		return
	}
	writeJSON(w, 200, map[string]any{"received": prefix, "complete": false})
}

// handleDeltaPut 实现 M3 additive delta：基底复制到会话临时映像、setLength(T)、
// 稀疏脏块覆盖；创建与 final 提交都校验 base，完成态同 UID 重传幂等。
func (fs *fakeServer) handleDeltaPut(w http.ResponseWriter, r *http.Request, n *fnode, uid, base string, start, total int64, body []byte, final bool) {
	up := fs.uploads[uid]
	if up != nil && up.done {
		writeJSON(w, 200, map[string]any{"received": total, "complete": true, "etag": up.etag})
		return
	}
	base = strings.Trim(base, "\"")
	if up == nil {
		if n.etag() != base {
			writeErr(w, 412, "PRECONDITION_FAILED", "base changed", false, nil)
			return
		}
		data := append([]byte(nil), n.data...)
		if total >= int64(len(data)) {
			data = append(data, make([]byte, total-int64(len(data)))...)
		} else {
			data = data[:total]
		}
		up = &uploadSession{chunks: map[int64][]byte{}, total: total, target: n.id, delta: true, base: base, data: data}
		fs.uploads[uid] = up
	}
	if n.etag() != up.base {
		writeErr(w, 412, "PRECONDITION_FAILED", "base changed", false, nil)
		return
	}
	up.chunks[start] = append([]byte(nil), body...)
	copy(up.data[start:start+int64(len(body))], body)
	if !final {
		writeJSON(w, 200, map[string]any{"received": start + int64(len(body)), "complete": false})
		return
	}
	n.data, n.logicalSize = append([]byte(nil), up.data...), 0
	if ms := r.Header.Get("X-Set-Mtime"); ms != "" {
		if v, err := strconv.ParseInt(ms, 10, 64); err == nil {
			n.modMs = v // 真实服务端在提交时原子应用 mtime
		}
	}
	up.done, up.etag = true, n.etag()
	if fs.deltaFinal503Once && !fs.deltaInjected {
		fs.deltaInjected = true
		writeErr(w, 503, "TEMPORARY", "response after commit", true, nil)
		return
	}
	writeJSON(w, 200, map[string]any{"received": total, "complete": true, "etag": up.etag})
}

// ---------- 测试设施 ----------

type testLogger struct{ t *testing.T }

func (l testLogger) Infof(f string, a ...any)  { l.t.Logf("INFO  "+f, a...) }
func (l testLogger) Warnf(f string, a ...any)  { l.t.Logf("WARN  "+f, a...) }
func (l testLogger) Errorf(f string, a ...any) { l.t.Logf("ERROR "+f, a...) }
func (l testLogger) Tracef(f string, a ...any) { l.t.Logf("TRACE "+f, a...) }

type fixture struct {
	fs  *fakeServer
	srv *httptest.Server
	cl  *httpfs.Client
	b   *httpfs.BridgeFS
}

func newFixture(t *testing.T, readOnly bool) *fixture {
	t.Helper()
	fs := newFakeServer()
	srv := httptest.NewServer(fs)
	t.Cleanup(srv.Close)
	lg := testLogger{t}
	cl := httpfs.New(srv.URL, "test-token", lg)
	b := httpfs.NewBridgeFS(cl, lg, false, readOnly)
	if err := b.Warmup(context.Background()); err != nil {
		t.Fatalf("warmup: %v", err)
	}
	return &fixture{fs: fs, srv: srv, cl: cl, b: b}
}

const dir = "/内部存储"

func (f *fixture) createWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	errc, fh := f.b.Create(path, 0, 0)
	if errc != 0 {
		t.Fatalf("Create %q errc=%d", path, errc)
	}
	if n := f.b.Write(path, data, 0, fh); n != len(data) {
		t.Fatalf("Write %q n=%d want %d", path, n, len(data))
	}
	if errc := f.b.Release(path, fh); errc != 0 {
		t.Fatalf("Release %q errc=%d", path, errc)
	}
}

func (f *fixture) fileByName(parent, name string) *fnode {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	return f.fs.child(parent, name)
}

// ---------- 用例 ----------

func TestCreateWriteReadbackAndDirCacheInvalidation(t *testing.T) {
	f := newFixture(t, false)
	path := dir + "/hello.txt"
	f.createWrite(t, path, []byte("hello m2"))

	n := f.fileByName("stor:primary", "hello.txt")
	if n == nil || string(n.data) != "hello m2" {
		t.Fatalf("server content mismatch: %+v", n)
	}
	// 写后父目录 dirCache 已失效：Readdir 触发新 list
	f.fs.mu.Lock()
	before := f.fs.listCnt
	f.fs.mu.Unlock()
	errc, fh := f.b.Opendir(dir)
	if errc != 0 {
		t.Fatalf("Opendir errc=%d", errc)
	}
	errc = f.b.Readdir(dir, func(name string, stat *fuse.Stat_t, ofst int64) bool { return true }, 0, fh)
	if errc != 0 {
		t.Fatalf("Readdir errc=%d", errc)
	}
	f.fs.mu.Lock()
	after := f.fs.listCnt
	f.fs.mu.Unlock()
	if after <= before {
		t.Fatalf("dirCache not invalidated: list count %d -> %d", before, after)
	}
	// Getattr 大小正确
	var st fuse.Stat_t
	if errc := f.b.Getattr(path, &st, ^uint64(0)); errc != 0 || st.Size != 8 {
		t.Fatalf("Getattr errc=%d size=%d", errc, st.Size)
	}
}

func TestLargeFileChunkedUpload(t *testing.T) {
	f := newFixture(t, false)
	data := make([]byte, 5<<20+123) // 5MiB+123B → 6 块（5×1MiB + 123B）
	_, _ = rand.Read(data)
	f.createWrite(t, dir+"/big.bin", data)

	n := f.fileByName("stor:primary", "big.bin")
	if n == nil || len(n.data) != len(data) {
		t.Fatalf("big file size mismatch: %v", len(n.data))
	}
	for i := range data {
		if n.data[i] != data[i] {
			t.Fatalf("big file content mismatch at %d", i)
		}
	}
	f.fs.mu.Lock()
	chunks := len(f.fs.chunkLog)
	firstUID := strings.Fields(f.fs.chunkLog[0])[0]
	uidConsistent := true
	for _, line := range f.fs.chunkLog {
		if strings.Fields(line)[0] != firstUID {
			uidConsistent = false
		}
	}
	f.fs.mu.Unlock()
	if chunks != 6 {
		t.Fatalf("chunk count=%d want 6 (log=%v)", chunks, f.fs.chunkLog)
	}
	if !uidConsistent {
		t.Fatalf("X-Upload-Id not consistent across chunks")
	}
}

func TestChunkResumeOnIncomplete(t *testing.T) {
	f := newFixture(t, false)
	f.fs.incompleteOnce = true  // 首块收下但回 409 received=1MiB
	data := make([]byte, 5<<20) // >4MiB 阈值 → 5 块，首块后按 received 续传
	_, _ = rand.Read(data)
	f.createWrite(t, dir+"/resume.bin", data)

	n := f.fileByName("stor:primary", "resume.bin")
	if n == nil || len(n.data) != len(data) {
		t.Fatalf("resume upload incomplete: size=%d", len(n.data))
	}
	for i := range data {
		if n.data[i] != data[i] {
			t.Fatalf("resume content mismatch at %d", i)
		}
	}
	// 客户端应从 received=1MiB 续传：第二请求 start=1MiB（而非重发 0 块后卡死）
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if len(f.fs.chunkLog) < 2 {
		t.Fatalf("chunkLog too short: %v", f.fs.chunkLog)
	}
	var start, end, total int64
	_, _ = fmt.Sscanf(strings.Fields(f.fs.chunkLog[1])[1], "%d-%d/%d", &start, &end, &total)
	if start != 1<<20 {
		t.Fatalf("resume start=%d want %d (log=%v)", start, 1<<20, f.fs.chunkLog)
	}
}

func TestChunkOutOfOrderAndLastChunkGap(t *testing.T) {
	f := newFixture(t, false)
	f.createWrite(t, dir+"/outoforder.bin", []byte("seed"))
	n := f.fileByName("stor:primary", "outoforder.bin")
	const total = 3 << 20
	data := make([]byte, total)
	_, _ = rand.Read(data)
	ctx := context.Background()
	// 中块先到：服务端 received 仍为 0，不能假装连续前缀已推进。
	received, complete, _, err := f.cl.PutChunk(ctx, n.id, "uid-outoforder", 1<<20, total, data[1<<20:2<<20], 0)
	if err != nil || complete || received != 0 {
		t.Fatalf("middle-first received=%d complete=%v err=%v", received, complete, err)
	}
	// 末块抢先到：缺口存在，必须 409 UPLOAD_INCOMPLETE received=0。
	received, complete, _, err = f.cl.PutChunk(ctx, n.id, "uid-outoforder", 2<<20, total, data[2<<20:], 0)
	var ae *httpfs.APIError
	if err == nil || !errors.As(err, &ae) || ae.Code != "UPLOAD_INCOMPLETE" || received != 0 || complete {
		t.Fatalf("early-last received=%d complete=%v err=%v", received, complete, err)
	}
	// 首块补齐后，中块与末块均已在会话内，连续水位直接到 total；
	// 但当前请求不是末块，complete 仍为 false，需重传末块触发提交。
	received, complete, _, err = f.cl.PutChunk(ctx, n.id, "uid-outoforder", 0, total, data[:1<<20], 0)
	if err != nil || complete || received != total {
		t.Fatalf("first-last received=%d complete=%v err=%v", received, complete, err)
	}
	// 同 uploadID+start 重传末块，完成组装且内容逐字节一致。
	received, complete, _, err = f.cl.PutChunk(ctx, n.id, "uid-outoforder", 2<<20, total, data[2<<20:], 0)
	if err != nil || !complete || received != total {
		t.Fatalf("final-last received=%d complete=%v err=%v", received, complete, err)
	}
	n = f.fileByName("stor:primary", "outoforder.bin")
	if n == nil || len(n.data) != total {
		t.Fatalf("out-of-order assembled size=%d", len(n.data))
	}
	for i := range data {
		if n.data[i] != data[i] {
			t.Fatalf("out-of-order content mismatch at %d", i)
		}
	}
}

func TestChunkIdempotentRetry503(t *testing.T) {
	f := newFixture(t, false)
	f.fs.retry503Once = true    // 首块第一次 503，重试同 uploadID+start
	data := make([]byte, 5<<20) // >4MiB 阈值，确保走分块路径
	_, _ = rand.Read(data)
	f.createWrite(t, dir+"/retry.bin", data)

	n := f.fileByName("stor:primary", "retry.bin")
	if n == nil || len(n.data) != len(data) {
		t.Fatalf("retry upload failed")
	}
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if len(f.fs.chunkLog) < 2 {
		t.Fatalf("expected retry, log=%v", f.fs.chunkLog)
	}
	// 前两条应是同 uploadID 同 Content-Range（幂等重传）
	if f.fs.chunkLog[0] != f.fs.chunkLog[1] {
		t.Fatalf("retry not idempotent: %q vs %q", f.fs.chunkLog[0], f.fs.chunkLog[1])
	}
}

func TestCreateConflict409(t *testing.T) {
	f := newFixture(t, false)
	f.createWrite(t, dir+"/dup.txt", []byte("x"))
	errc, _ := f.b.Create(dir+"/dup.txt", 0, 0)
	if errc != -fuse.EEXIST {
		t.Fatalf("duplicate create errc=%d want EEXIST(%d)", errc, -fuse.EEXIST)
	}
}

func TestRenameDirENOSYSAndPatchMeta501(t *testing.T) {
	f := newFixture(t, false)
	if errc := f.b.Mkdir(dir+"/dirA", 0); errc != 0 {
		t.Fatalf("Mkdir errc=%d", errc)
	}
	if errc := f.b.Rename(dir+"/dirA", dir+"/dirB"); errc != -fuse.ENOSYS {
		t.Fatalf("dir rename errc=%d want ENOSYS", errc)
	}
	// 直接 PatchMeta 目录 → 服务端 501
	d := f.fileByName("stor:primary", "dirA")
	name := "dirB"
	_, err := f.cl.PatchMeta(context.Background(), d.id, &name, nil, nil, false)
	if err == nil || !strings.Contains(err.Error(), "501") {
		t.Fatalf("PatchMeta dir err=%v want 501", err)
	}
}

func TestRenameFileAndMove(t *testing.T) {
	f := newFixture(t, false)
	f.createWrite(t, dir+"/a.txt", []byte("aaa"))
	// 同目录改名
	if errc := f.b.Rename(dir+"/a.txt", dir+"/b.txt"); errc != 0 {
		t.Fatalf("rename errc=%d", errc)
	}
	var st fuse.Stat_t
	if errc := f.b.Getattr(dir+"/a.txt", &st, ^uint64(0)); errc != -fuse.ENOENT {
		t.Fatalf("old path still resolves errc=%d", errc)
	}
	if errc := f.b.Getattr(dir+"/b.txt", &st, ^uint64(0)); errc != 0 || st.Size != 3 {
		t.Fatalf("new path getattr errc=%d size=%d", errc, st.Size)
	}
	// 跨目录移动
	if errc := f.b.Mkdir(dir+"/sub", 0); errc != 0 {
		t.Fatalf("Mkdir sub errc=%d", errc)
	}
	if errc := f.b.Rename(dir+"/b.txt", dir+"/SUB/c.txt"); errc != 0 {
		t.Fatalf("move errc=%d", errc)
	}
	if n := f.fileByName("stor:primary", "b.txt"); n != nil {
		t.Fatalf("b.txt still at old parent")
	}
	sub := f.fileByName("stor:primary", "sub")
	if n := f.fileByName(sub.id, "c.txt"); n == nil || string(n.data) != "aaa" {
		t.Fatalf("c.txt not moved correctly")
	}
	// rename + 跨目录 move 后，新路径必须可 Getattr/Utimens（真机 harness 10）。
	if errc := f.b.Getattr(dir+"/sub/c.txt", &st, ^uint64(0)); errc != 0 {
		t.Fatalf("moved path Getattr errc=%d", errc)
	}
	ts := []fuse.Timespec{{Sec: 0, Nsec: 0}, {Sec: 1700000001, Nsec: 0}}
	if errc := f.b.Utimens(dir+"/sub/c.txt", ts); errc != 0 {
		t.Fatalf("moved path Utimens errc=%d", errc)
	}
	if errc := f.b.Getattr(dir+"/sub/c.txt", &st, ^uint64(0)); errc != 0 || st.Mtim.Sec != 1700000001 {
		t.Fatalf("moved path mtime errc=%d sec=%d", errc, st.Mtim.Sec)
	}
}

func TestDeleteNotEmpty(t *testing.T) {
	f := newFixture(t, false)
	if errc := f.b.Mkdir(dir+"/nonempty", 0); errc != 0 {
		t.Fatalf("Mkdir errc=%d", errc)
	}
	f.createWrite(t, dir+"/nonempty/f.txt", []byte("x"))
	if errc := f.b.Rmdir(dir + "/nonempty"); errc != -fuse.ENOTEMPTY {
		t.Fatalf("rmdir nonempty errc=%d want ENOTEMPTY", errc)
	}
}

func TestDeletePartial(t *testing.T) {
	f := newFixture(t, false)
	if errc := f.b.Mkdir(dir+"/partial-dir", 0); errc != 0 {
		t.Fatalf("Mkdir errc=%d", errc)
	}
	if errc := f.b.Rmdir(dir + "/partial-dir"); errc != -fuse.EIO {
		t.Fatalf("rmdir partial errc=%d want EIO", errc)
	}
}

func TestDeleteIdempotent404(t *testing.T) {
	f := newFixture(t, false)
	if errc := f.b.Unlink(dir + "/no-such-file"); errc != 0 {
		t.Fatalf("unlink missing errc=%d want 0 (idempotent)", errc)
	}
}

func TestUnlinkRemovesNode(t *testing.T) {
	f := newFixture(t, false)
	f.createWrite(t, dir+"/gone.txt", []byte("x"))
	if errc := f.b.Unlink(dir + "/gone.txt"); errc != 0 {
		t.Fatalf("unlink errc=%d", errc)
	}
	if n := f.fileByName("stor:primary", "gone.txt"); n != nil {
		t.Fatalf("server still has gone.txt")
	}
	var st fuse.Stat_t
	if errc := f.b.Getattr(dir+"/gone.txt", &st, ^uint64(0)); errc != -fuse.ENOENT {
		t.Fatalf("gone.txt getattr errc=%d want ENOENT", errc)
	}
}

func TestTruncateZeroAndNonZeroENOSYS(t *testing.T) {
	f := newFixture(t, false)
	path := dir + "/trunc.txt"
	errc, fh := f.b.Create(path, 0, 0)
	if errc != 0 {
		t.Fatalf("Create errc=%d", errc)
	}
	if n := f.b.Write(path, []byte("abcdef"), 0, fh); n != 6 {
		t.Fatalf("Write n=%d", n)
	}
	if errc := f.b.Truncate(path, 5, fh); errc != -fuse.ENOSYS {
		t.Fatalf("truncate nonzero errc=%d want ENOSYS (P2-12)", errc)
	}
	if errc := f.b.Truncate(path, 0, fh); errc != 0 {
		t.Fatalf("truncate zero errc=%d", errc)
	}
	if errc := f.b.Release(path, fh); errc != 0 {
		t.Fatalf("Release errc=%d", errc)
	}
	if n := f.fileByName("stor:primary", "trunc.txt"); n == nil || len(n.data) != 0 {
		t.Fatalf("truncate zero not uploaded: %+v", n)
	}
}

func TestReadOnlyRefusals(t *testing.T) {
	f := newFixture(t, true) // -ro
	// 已存在文件的写打开必须 EROFS（不存在路径按 M1 语义仍 ENOENT）。
	f.fs.mu.Lock()
	f.fs.nodes["p:ro"] = &fnode{id: "p:ro", parent: "stor:primary", name: "ro.txt", data: []byte("x"), modMs: 1000}
	f.fs.mu.Unlock()
	if errc, _ := f.b.Create(dir+"/ro.txt", 0, 0); errc != -fuse.EROFS {
		t.Fatalf("ro Create errc=%d want EROFS", errc)
	}
	if errc := f.b.Mkdir(dir+"/rodir", 0); errc != -fuse.EROFS {
		t.Fatalf("ro Mkdir errc=%d want EROFS", errc)
	}
	if errc := f.b.Unlink(dir + "/ro.txt"); errc != -fuse.EROFS {
		t.Fatalf("ro Unlink errc=%d want EROFS", errc)
	}
	if errc := f.b.Rename(dir+"/a", dir+"/b"); errc != -fuse.EROFS {
		t.Fatalf("ro Rename errc=%d want EROFS", errc)
	}
	if errc, _ := f.b.Open(dir+"/ro.txt", fuse.O_WRONLY|fuse.O_CREAT); errc != -fuse.EROFS {
		t.Fatalf("ro Open write errc=%d want EROFS", errc)
	}
	if errc := f.b.Utimens(dir+"/ro.txt", nil); errc != -fuse.EROFS {
		t.Fatalf("ro Utimens errc=%d want EROFS", errc)
	}
	// Access 拒写
	if errc := f.b.Access(dir, fuse.W_OK); errc != -fuse.EACCES {
		t.Fatalf("ro Access W_OK errc=%d want EACCES", errc)
	}
}

func TestUtimensSetsMtime(t *testing.T) {
	f := newFixture(t, false)
	f.createWrite(t, dir+"/time.txt", []byte("x"))
	ts := []fuse.Timespec{{Sec: 1000, Nsec: 0}, {Sec: 1700000000, Nsec: 0}}
	if errc := f.b.Utimens(dir+"/time.txt", ts); errc != 0 {
		t.Fatalf("Utimens errc=%d", errc)
	}
	var st fuse.Stat_t
	if errc := f.b.Getattr(dir+"/time.txt", &st, ^uint64(0)); errc != 0 {
		t.Fatalf("Getattr errc=%d", errc)
	}
	if st.Mtim.Sec != 1700000000 {
		t.Fatalf("mtime=%d want 1700000000", st.Mtim.Sec)
	}
}

// P0-1 回归：Windows 复制/覆盖常在同一写句柄周期内 SetFileTime。Utimens 不能先
// PATCH 服务端 mtime（会改 ETag 打断 delta 基线），否则 final 412/EBUSY、覆盖写丢失。
func TestUtimensDuringOpenWriteKeepsDeltaBaseline(t *testing.T) {
	f := newFixture(t, false)
	path := dir + "/overwrite.txt"
	f.createWrite(t, path, []byte("old"))

	errc, fh := f.b.Open(path, fuse.O_RDWR|fuse.O_TRUNC)
	if errc != 0 {
		t.Fatalf("Open O_TRUNC errc=%d", errc)
	}
	if n := f.b.Write(path, []byte("new"), 0, fh); n != 3 {
		t.Fatalf("Write n=%d want 3", n)
	}
	// SetFileTimeUtc 会另开一个写属性句柄；它先 Release 时不得抢先 PATCH mtime，
	// 否则打断数据写句柄的 delta 基线（真机复现路径）。
	errc, attrs := f.b.Open(path, fuse.O_WRONLY)
	if errc != 0 {
		t.Fatalf("Open attrs errc=%d", errc)
	}
	ts := []fuse.Timespec{{Sec: 1000}, {Sec: 1700000100}}
	if errc := f.b.Utimens(path, ts); errc != 0 {
		t.Fatalf("Utimens during write errc=%d", errc)
	}
	if errc := f.b.Release(path, attrs); errc != 0 {
		t.Fatalf("Release attrs errc=%d", errc)
	}
	if errc := f.b.Flush(path, fh); errc != 0 {
		t.Fatalf("Flush errc=%d (delta baseline broken -> data loss)", errc)
	}
	if errc := f.b.Release(path, fh); errc != 0 {
		t.Fatalf("Release errc=%d", errc)
	}

	n := f.fileByName("stor:primary", "overwrite.txt")
	if n == nil || string(n.data) != "new" {
		t.Fatalf("server content=%q want new (overwrite lost)", n.data)
	}
	if n.modMs != 1700000100000 {
		t.Fatalf("server mtime=%d want 1700000100000", n.modMs)
	}
}

func TestWriteAtOffsetPreservesOriginal(t *testing.T) {
	f := newFixture(t, false)
	path := dir + "/cow.txt"
	f.createWrite(t, path, []byte("0123456789"))
	// 非截断写打开：O_RDWR 不 O_TRUNC → ensureLoaded 拉原文，偏移写保留其余
	errc, fh := f.b.Open(path, fuse.O_RDWR)
	if errc != 0 {
		t.Fatalf("Open errc=%d", errc)
	}
	if n := f.b.Write(path, []byte("XY"), 3, fh); n != 2 {
		t.Fatalf("Write n=%d", n)
	}
	if errc := f.b.Release(path, fh); errc != 0 {
		t.Fatalf("Release errc=%d", errc)
	}
	if n := f.fileByName("stor:primary", "cow.txt"); n == nil || string(n.data) != "012XY56789" {
		t.Fatalf("copy-on-write content=%q want 012XY56789", n.data)
	}
}

func TestOtherOpenHandleCacheInvalidation(t *testing.T) {
	f := newFixture(t, false)
	path := dir + "/shared.txt"
	f.createWrite(t, path, []byte("0123456789"))

	errc, reader := f.b.Open(path, fuse.O_RDONLY)
	if errc != 0 {
		t.Fatalf("open reader errc=%d", errc)
	}
	before := make([]byte, 4)
	if n := f.b.Read(path, before, 0, reader); n != 4 || string(before) != "0123" {
		t.Fatalf("initial read n=%d data=%q", n, before)
	}
	errc, writer := f.b.Open(path, fuse.O_WRONLY)
	if errc != 0 {
		t.Fatalf("open writer errc=%d", errc)
	}
	if n := f.b.Write(path, []byte("ABCD"), 0, writer); n != 4 {
		t.Fatalf("write n=%d", n)
	}
	if errc := f.b.Flush(path, writer); errc != 0 {
		t.Fatalf("flush writer errc=%d", errc)
	}
	after := make([]byte, 4)
	if n := f.b.Read(path, after, 0, reader); n != 4 || string(after) != "ABCD" {
		t.Fatalf("existing reader stale: n=%d data=%q want ABCD", n, after)
	}
	if errc := f.b.Release(path, writer); errc != 0 {
		t.Fatalf("release writer errc=%d", errc)
	}
	if errc := f.b.Release(path, reader); errc != 0 {
		t.Fatalf("release reader errc=%d", errc)
	}
}

func TestChunkLogContentRangeFormat(t *testing.T) {
	// 直接客户端级：PutChunk 头部格式与 409 received 解析
	f := newFixture(t, false)
	f.createWrite(t, dir+"/hdr.bin", []byte("seed"))
	n := f.fileByName("stor:primary", "hdr.bin")
	chunk := []byte("0123456789")
	received, complete, _, err := f.cl.PutChunk(context.Background(), n.id, "uid-test", 0, 10, chunk, 1700000000000)
	if err != nil || !complete || received != 10 {
		t.Fatalf("PutChunk single-block received=%d complete=%v err=%v", received, complete, err)
	}
	f.fs.mu.Lock()
	line := f.fs.chunkLog[len(f.fs.chunkLog)-1]
	f.fs.mu.Unlock()
	if line != "uid-test 0-9/10" {
		t.Fatalf("Content-Range log=%q want 'uid-test 0-9/10'", line)
	}
}

func TestServerQuota507(t *testing.T) {
	// 507 映射 ENOSPC：假服务端整写不触发 507，这里只校验 apiErrno 映射表经
	// 由 upload 路径——直接构造 507 响应的轻量服务端
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/roots" {
			writeJSON(w, 200, map[string]any{"roots": []any{map[string]any{
				"id": "stor:primary", "parent": "root", "name": "内部存储", "is_dir": true, "writable": true}}})
			return
		}
		if r.URL.Path == "/v1/fs/statfs" {
			writeJSON(w, 200, map[string]any{"total_bytes": 1, "free_bytes": 0, "block_size": 4096})
			return
		}
		writeErr(w, 507, "QUOTA_EXCEEDED", "no space", false, nil)
	}))
	defer srv.Close()
	lg := testLogger{t}
	cl := httpfs.New(srv.URL, "tok", lg)
	_, err := cl.PutContent(context.Background(), "p:1", []byte("x"), 0)
	if err == nil {
		t.Fatal("expected 507 error")
	}
	if !strings.Contains(err.Error(), "507") {
		t.Fatalf("err=%v", err)
	}
}

func TestDeltaNonAlignedWriteExtendAndReadConsistency(t *testing.T) {
	f := newFixture(t, false)
	path := dir + "/delta.bin"
	f.createWrite(t, path, []byte("0123456789"))

	errc, reader := f.b.Open(path, fuse.O_RDONLY)
	if errc != 0 {
		t.Fatalf("open reader errc=%d", errc)
	}
	cached := make([]byte, 4)
	if n := f.b.Read(path, cached, 0, reader); n != 4 || string(cached) != "0123" {
		t.Fatalf("prime reader n=%d data=%q", n, cached)
	}
	errc, writer := f.b.Open(path, fuse.O_RDWR)
	if errc != 0 {
		t.Fatalf("open writer errc=%d", errc)
	}
	if n := f.b.Write(path, []byte("XY"), 3, writer); n != 2 {
		t.Fatalf("non-aligned write n=%d", n)
	}
	if n := f.b.Write(path, []byte("Z"), 20, writer); n != 1 {
		t.Fatalf("extending write n=%d", n)
	}
	want := []byte("012XY56789")
	want = append(want, make([]byte, 20-len(want))...)
	want = append(want, 'Z')
	beforeFlush := make([]byte, len(want))
	if n := f.b.Read(path, beforeFlush, 0, writer); n != len(want) || !bytes.Equal(beforeFlush, want) {
		t.Fatalf("write-handle read n=%d data=%v want=%v", n, beforeFlush, want)
	}
	if errc := f.b.Flush(path, writer); errc != 0 {
		t.Fatalf("delta flush errc=%d", errc)
	}
	afterFlush := make([]byte, len(want))
	if n := f.b.Read(path, afterFlush, 0, reader); n != len(want) || !bytes.Equal(afterFlush, want) {
		t.Fatalf("other reader n=%d data=%v want=%v", n, afterFlush, want)
	}
	if n := f.fileByName("stor:primary", "delta.bin"); n == nil || !bytes.Equal(n.data, want) {
		t.Fatalf("server delta content=%v want=%v", n.data, want)
	}
	f.fs.mu.Lock()
	logs := append([]deltaRecord(nil), f.fs.deltaLog...)
	f.fs.mu.Unlock()
	if len(logs) != 2 || logs[0].start != 3 || logs[0].end != 4 || logs[0].final ||
		logs[1].start != 20 || logs[1].end != 20 || !logs[1].final {
		t.Fatalf("delta ranges=%+v", logs)
	}
	for _, r := range logs {
		if r.total != 21 || r.base != "\"10-1000\"" {
			t.Fatalf("delta contract record=%+v", r)
		}
	}
	if errc := f.b.Release(path, writer); errc != 0 {
		t.Fatalf("release writer errc=%d", errc)
	}
	if errc := f.b.Release(path, reader); errc != 0 {
		t.Fatalf("release reader errc=%d", errc)
	}
}

func TestDeltaDirtyIntervalMergeAndSecondFlush(t *testing.T) {
	f := newFixture(t, false)
	path := dir + "/merge.bin"
	f.createWrite(t, path, []byte("abcdefghijk"))
	errc, fh := f.b.Open(path, fuse.O_RDWR)
	if errc != 0 {
		t.Fatalf("open errc=%d", errc)
	}
	for _, w := range []struct {
		data string
		off  int64
	}{{"bc", 1}, {"de", 3}, {"Z", 2}, {"Q", 10}} {
		if n := f.b.Write(path, []byte(w.data), w.off, fh); n != len(w.data) {
			t.Fatalf("write %q n=%d", w.data, n)
		}
	}
	if errc := f.b.Flush(path, fh); errc != 0 {
		t.Fatalf("first flush errc=%d", errc)
	}
	f.fs.mu.Lock()
	first := append([]deltaRecord(nil), f.fs.deltaLog...)
	f.fs.mu.Unlock()
	if len(first) != 2 || first[0].start != 1 || first[0].end != 4 || first[0].final ||
		first[1].start != 10 || first[1].end != 10 || !first[1].final {
		t.Fatalf("merged ranges=%+v", first)
	}
	if n := f.b.Write(path, []byte("!"), 6, fh); n != 1 {
		t.Fatalf("second-round write n=%d", n)
	}
	if errc := f.b.Flush(path, fh); errc != 0 {
		t.Fatalf("second flush errc=%d", errc)
	}
	if errc := f.b.Flush(path, fh); errc != 0 { // 无脏块不得重传
		t.Fatalf("clean flush errc=%d", errc)
	}
	f.fs.mu.Lock()
	all := append([]deltaRecord(nil), f.fs.deltaLog...)
	f.fs.mu.Unlock()
	if len(all) != 3 {
		t.Fatalf("second flush resent clean blocks: %+v", all)
	}
	if all[2].start != 6 || all[2].end != 6 || !all[2].final || all[2].uid == all[1].uid || all[2].base != "\"11-1000\"" {
		t.Fatalf("second-round delta=%+v previous=%+v", all[2], all[1])
	}
	if n := f.fileByName("stor:primary", "merge.bin"); n == nil || string(n.data) != "abZdef!hijQ" {
		t.Fatalf("merged content=%q", n.data)
	}
	if errc := f.b.Release(path, fh); errc != 0 {
		t.Fatalf("release errc=%d", errc)
	}
}

func TestDeltaTruncatedExistingCoversZeroHoles(t *testing.T) {
	f := newFixture(t, false)
	path := dir + "/truncate-delta.bin"
	f.createWrite(t, path, []byte("abcdefghij"))
	errc, fh := f.b.Open(path, fuse.O_WRONLY|fuse.O_TRUNC)
	if errc != 0 {
		t.Fatalf("open trunc errc=%d", errc)
	}
	if n := f.b.Write(path, []byte("Z"), 3, fh); n != 1 {
		t.Fatalf("write after trunc n=%d", n)
	}
	if errc := f.b.Flush(path, fh); errc != 0 {
		t.Fatalf("flush trunc delta errc=%d", errc)
	}
	f.fs.mu.Lock()
	logs := append([]deltaRecord(nil), f.fs.deltaLog...)
	f.fs.mu.Unlock()
	if len(logs) != 1 || logs[0].start != 0 || logs[0].end != 3 || logs[0].total != 4 ||
		logs[0].base != "\"10-1000\"" || !logs[0].final {
		t.Fatalf("truncated delta did not cover zero holes: %+v", logs)
	}
	if n := f.fileByName("stor:primary", "truncate-delta.bin"); n == nil || !bytes.Equal(n.data, []byte{0, 0, 0, 'Z'}) {
		t.Fatalf("truncated delta content=%v", n.data)
	}
	if errc := f.b.Release(path, fh); errc != 0 {
		t.Fatalf("release errc=%d", errc)
	}
}

func TestDeltaFinalCompletedRetry(t *testing.T) {
	f := newFixture(t, false)
	f.fs.deltaFinal503Once = true
	path := dir + "/final-retry.bin"
	f.createWrite(t, path, []byte("original"))
	errc, fh := f.b.Open(path, fuse.O_WRONLY)
	if errc != 0 {
		t.Fatalf("open errc=%d", errc)
	}
	if n := f.b.Write(path, []byte("NEW"), 2, fh); n != 3 {
		t.Fatalf("write n=%d", n)
	}
	if errc := f.b.Flush(path, fh); errc != 0 {
		t.Fatalf("flush with completed-final retry errc=%d", errc)
	}
	f.fs.mu.Lock()
	logs := append([]deltaRecord(nil), f.fs.deltaLog...)
	f.fs.mu.Unlock()
	if len(logs) != 2 || !logs[0].final || !logs[1].final || logs[0] != logs[1] {
		t.Fatalf("final was not retransmitted identically: %+v", logs)
	}
	if n := f.fileByName("stor:primary", "final-retry.bin"); n == nil || string(n.data) != "orNEWnal" {
		t.Fatalf("retry content=%q", n.data)
	}
	if errc := f.b.Release(path, fh); errc != 0 {
		t.Fatalf("release errc=%d", errc)
	}
}

func TestDeltaPreconditionFailureKeepsDirtyAndTarget(t *testing.T) {
	f := newFixture(t, false)
	path := dir + "/conflict.bin"
	original := []byte("0123456789")
	f.createWrite(t, path, original)
	errc, fh := f.b.Open(path, fuse.O_RDWR)
	if errc != 0 {
		t.Fatalf("open errc=%d", errc)
	}
	one := make([]byte, 1)
	if n := f.b.Read(path, one, 0, fh); n != 1 { // 固定本次写句柄的 base
		t.Fatalf("prime read n=%d", n)
	}
	n := f.fileByName("stor:primary", "conflict.bin")
	f.fs.mu.Lock()
	n.data, n.modMs = []byte("outside"), 2000
	f.fs.mu.Unlock()
	if n := f.b.Write(path, []byte("Z"), 1, fh); n != 1 {
		t.Fatalf("write n=%d", n)
	}
	if errc := f.b.Flush(path, fh); errc != -fuse.EBUSY {
		t.Fatalf("conflicting flush errc=%d want EBUSY", errc)
	}
	if got := f.fileByName("stor:primary", "conflict.bin"); string(got.data) != "outside" {
		t.Fatalf("412 damaged concurrent target: %q", got.data)
	}
	// 恢复原基底后再次 Flush：若 dirty 丢失，内容不会包含 Z。
	f.fs.mu.Lock()
	n.data, n.modMs = append([]byte(nil), original...), 1000
	f.fs.mu.Unlock()
	if errc := f.b.Flush(path, fh); errc != 0 {
		t.Fatalf("retry after 412 errc=%d (dirty blocks lost)", errc)
	}
	if got := f.fileByName("stor:primary", "conflict.bin"); string(got.data) != "0Z23456789" {
		t.Fatalf("retry content=%q", got.data)
	}
	if errc := f.b.Release(path, fh); errc != 0 {
		t.Fatalf("release errc=%d", errc)
	}
}

func TestRenameReplaceDoesNotPreDelete(t *testing.T) {
	f := newFixture(t, false)
	f.createWrite(t, dir+"/source.txt", []byte("source"))
	f.createWrite(t, dir+"/target.txt", []byte("target"))
	source := f.fileByName("stor:primary", "source.txt")
	target := f.fileByName("stor:primary", "target.txt")
	sourceID, targetID := source.id, target.id
	f.fs.mu.Lock()
	f.fs.opLog = nil
	f.fs.mu.Unlock()
	if errc := f.b.Rename(dir+"/source.txt", dir+"/target.txt"); errc != 0 {
		t.Fatalf("rename replace errc=%d", errc)
	}
	f.fs.mu.Lock()
	ops := append([]string(nil), f.fs.opLog...)
	_, oldTargetExists := f.fs.nodes[targetID]
	f.fs.mu.Unlock()
	if len(ops) != 1 || ops[0] != "patch replace=true" {
		t.Fatalf("rename operation order=%v", ops)
	}
	if oldTargetExists {
		t.Fatal("replaced target id still exists")
	}
	got := f.fileByName("stor:primary", "target.txt")
	if got == nil || got.id != sourceID || string(got.data) != "source" {
		t.Fatalf("atomic replacement result=%+v", got)
	}
	f.createWrite(t, dir+"/other.txt", []byte("other"))
	other := f.fileByName("stor:primary", "other.txt")
	name := "target.txt"
	_, err := f.cl.PatchMeta(context.Background(), other.id, &name, nil, nil, false)
	var ae *httpfs.APIError
	if err == nil || !errors.As(err, &ae) || ae.HTTPStatus != 409 {
		t.Fatalf("ordinary rename conflict err=%v want 409", err)
	}
	if got := f.fileByName("stor:primary", "target.txt"); got == nil || got.id != sourceID || string(got.data) != "source" {
		t.Fatalf("409 changed target=%+v", got)
	}
}

func TestReleaseAndDestroyCleanupTempFiles(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("LOCALAPPDATA", cache)
	f := newFixture(t, false)
	tmpRoot := filepath.Join(cache, "PhoneFS", "tmp")
	other := filepath.Join(tmpRoot, "other-session")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(other, "keep")
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	errc, fh := f.b.Create(dir+"/temp-cleanup.bin", 0, 0)
	if errc != 0 {
		t.Fatalf("create errc=%d", errc)
	}
	entries, err := os.ReadDir(tmpRoot)
	if err != nil || len(entries) != 2 { // 当前随机会话 + other-session
		t.Fatalf("temp sessions entries=%v err=%v", entries, err)
	}
	var session string
	for _, e := range entries {
		if e.Name() != "other-session" {
			session = filepath.Join(tmpRoot, e.Name())
		}
	}
	files, err := filepath.Glob(filepath.Join(session, "write-*"))
	if err != nil || len(files) != 1 {
		t.Fatalf("open temp files=%v err=%v", files, err)
	}
	if errc := f.b.Release(dir+"/temp-cleanup.bin", fh); errc != 0 {
		t.Fatalf("release errc=%d", errc)
	}
	files, _ = filepath.Glob(filepath.Join(session, "write-*"))
	if len(files) != 0 {
		t.Fatalf("release left temp files=%v", files)
	}
	f.b.Destroy()
	if _, err := os.Stat(session); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mount session still exists: %s err=%v", session, err)
	}
	if data, err := os.ReadFile(keep); err != nil || string(data) != "keep" {
		t.Fatalf("broad cleanup touched other session: data=%q err=%v", data, err)
	}
}

func TestLargeLogicalFileUsesBoundedHeap(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("LOCALAPPDATA", cache)
	f := newFixture(t, false)
	const size = int64(2<<30 + 123)
	path := dir + "/sparse-2g.bin"
	f.fs.mu.Lock()
	f.fs.nodes["p:sparse"] = &fnode{id: "p:sparse", parent: "stor:primary", name: "sparse-2g.bin", logicalSize: size, modMs: 1000}
	f.fs.mu.Unlock()
	errc, fh := f.b.Open(path, fuse.O_RDWR)
	if errc != 0 {
		t.Fatalf("open sparse errc=%d", errc)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if n := f.b.Write(path, []byte{0x7f}, size-1, fh); n != 1 {
		t.Fatalf("large logical write n=%d", n)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	const allowance = 128 << 20
	if after.HeapAlloc > before.HeapAlloc+allowance {
		t.Fatalf("heap grew by %d bytes for %d-byte file", after.HeapAlloc-before.HeapAlloc, size)
	}
	got := make([]byte, 1)
	if n := f.b.Read(path, got, size-1, fh); n != 1 || got[0] != 0x7f {
		t.Fatalf("readback n=%d data=%v", n, got)
	}
	sessions, err := filepath.Glob(filepath.Join(cache, "PhoneFS", "tmp", "mount-*", "write-*"))
	if err != nil || len(sessions) != 1 {
		t.Fatalf("large temp files=%v err=%v", sessions, err)
	}
	st, err := os.Stat(sessions[0])
	if err != nil {
		t.Fatalf("stat large temp: %v", err)
	}
	if st.Size() != size {
		t.Fatalf("large temp size=%d want=%d", st.Size(), size)
	}
	// 不在此 Release：本用例只验证加载/读写的固定内存，避免假服务端为 final
	// 组装 2GiB 目标。Destroy 必须清理仍在写入的句柄和整个随机会话目录。
	f.b.Destroy()
	if _, err := os.Stat(filepath.Dir(sessions[0])); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("large temp session not cleaned: err=%v", err)
	}
}
