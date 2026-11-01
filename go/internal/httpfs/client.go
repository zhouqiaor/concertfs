// Package httpfs 把 PhoneFS 安卓端 HTTPS v1 协议封装成只读 Go API，
// 供 M1 cgofuse 桥（cmd/phonefs-m1）消费。
//
// 已真机核实的服务端契约（TEQU-S2C, 0.1.0-w2, 2026-11-01）：
//
//   - BaseURL 默认 https://192.168.43.1:18443；TLS1.3 自签 P-256，
//     M1 用 InsecureSkipVerify，W3 前换 pair/info 的 fingerprint 做证书钉死。
//   - 认证头 Authorization: Bearer <token>；缺失/错误 → 401 TOKEN_INVALID。
//   - GET /v1/pair/info                无需认证（指纹/设备名）。
//   - GET /v1/roots                    8 个固定根（7 个 cat:* + stor:primary）。
//   - GET /v1/fs/list?id=<entryId>     目录条目；cursor W2 不支持（传了 400）。
//     id=cat:* → 501 NOT_SUPPORTED（M1 分类后端未实现，桥按空目录呈现）。
//   - GET /v1/fs/statfs                {total_bytes,free_bytes,block_size}。
//   - GET /v1/fs/content?id=<p:...>    Range: bytes=a-b → 206 + Content-Range；
//     越界 → 416；不带 Range → 200 全量（桥永远带 Range，避免整文件进内存）。
//
// 已知 conformance 债（W3 对齐 SPEC 前勿改）：路径 /v1/roots（SPEC 草案
// /v1/fs/roots）、端口 18443（SPEC 62110）。路径常量集中在本文件。
package httpfs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL 是 W2 真机默认监听地址（站点本地网段 192.168.43.0/24）。
const DefaultBaseURL = "https://192.168.43.1:18443"

const (
	pathPairInfo = "/v1/pair/info"
	pathRoots    = "/v1/roots" // SPEC 债：草案为 /v1/fs/roots
	pathList     = "/v1/fs/list"
	pathStatfs   = "/v1/fs/statfs"
	pathContent  = "/v1/fs/content"
)

// 超时按「单次尝试」计；底层失败会再重试一次，故最坏延迟约 2 倍。
const (
	metadataTimeout = 20 * time.Second // roots/list/statfs/pair
	contentTimeout  = 30 * time.Second // 单块 1MiB 随机读
	retryBackoff    = 300 * time.Millisecond
)

// Logger 是桥接层日志接口（cmd 侧的 levelLogger 结构满足它）。
type Logger interface {
	Infof(format string, a ...any)
	Warnf(format string, a ...any)
	Errorf(format string, a ...any)
	Tracef(format string, a ...any)
}

// Entry 是 roots/list 共用的条目 DTO（服务端 FileInfo/root 同构）。
type Entry struct {
	ID         string `json:"id"`
	Parent     string `json:"parent"`
	Name       string `json:"name"`
	IsDir      bool   `json:"is_dir"`
	Size       int64  `json:"size"`
	ModifiedMs int64  `json:"modified_ms"`
	Mime       string `json:"mime"`
	Writable   bool   `json:"writable"`
}

// FSInfo 是 statfs 归一化结果。
type FSInfo struct {
	TotalBytes int64
	FreeBytes  int64
	BlockSize  int64
}

// PairInfo 是 /v1/pair/info 的最小字段集（指纹留作 W3 证书钉死）。
type PairInfo struct {
	Service          string `json:"service"`
	Fingerprint      string `json:"fingerprint"`
	DeviceName       string `json:"device.name"`
	DeviceFormFactor string `json:"device.form_factor"`
}

// APIError 是 4xx/5xx 结构化错误（body 形如 {"error":{...}}）。
// Received 在 409 UPLOAD_INCOMPLETE 时给出续传水位（details.received）；
// Details 保留原始 details 对象（如 409 PARTIAL 的 deleted/failed）。
type APIError struct {
	HTTPStatus int
	Code       string
	Message    string
	TraceID    string
	Retriable  bool
	Received   int64
	Details    map[string]any
}

func (e *APIError) Error() string {
	return fmt.Sprintf("phonefs http %d %s: %s (trace=%s)", e.HTTPStatus, e.Code, e.Message, e.TraceID)
}

// IsNotSupported 报告是否为 cat:* 列表的 501 NOT_SUPPORTED。
func IsNotSupported(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.HTTPStatus == http.StatusNotImplemented
}

type rootsResp struct {
	Roots []Entry `json:"roots"`
}

type listResp struct {
	Entries    []Entry `json:"entries"`
	NextCursor string  `json:"next_cursor"`
	Revision   int64   `json:"revision"`
}

type statfsResp struct {
	TotalBytes int64 `json:"total_bytes"`
	FreeBytes  int64 `json:"free_bytes"`
	BlockSize  int64 `json:"block_size"`
}

type errBody struct {
	Error struct {
		Code      string         `json:"code"`
		Message   string         `json:"message"`
		TraceID   string         `json:"trace_id"`
		Retriable bool           `json:"retriable"`
		Details   map[string]any `json:"details"`
	} `json:"error"`
}

// Client 是 PhoneFS HTTPS v1 的只读客户端。
type Client struct {
	baseURL string
	token   string
	httpc   *http.Client
	log     Logger
}

// New 构造客户端。baseURL 为空用 DefaultBaseURL；logger 不可为 nil。
func New(baseURL, token string, log Logger) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // M1 自签，W3 改指纹钉死
	transport.TLSClientConfig.MinVersion = 0x0303       // TLS1.2 下限（实测 1.3）
	transport.MaxIdleConnsPerHost = 16
	transport.IdleConnTimeout = 30 * time.Second
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		httpc:   &http.Client{Transport: transport}, // 不用整体 Timeout：按请求 context 控制
		log:     log,
	}
}

// PairInfo 拉取配对信息（无需 token；失败不致命，仅诊断用）。
func (c *Client) PairInfo(ctx context.Context) (PairInfo, error) {
	var out PairInfo
	body, err := c.getJSON(ctx, pathPairInfo, nil)
	if err != nil {
		return out, err
	}
	// device 是嵌套对象，先解通用 map 取 device.name，再解平字段。
	var raw struct {
		Service     string `json:"service"`
		Fingerprint string `json:"fingerprint"`
		Device      struct {
			Name       string `json:"name"`
			FormFactor string `json:"form_factor"`
		} `json:"device"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return out, fmt.Errorf("decode pair/info: %w", err)
	}
	out.Service, out.Fingerprint, out.DeviceName = raw.Service, raw.Fingerprint, raw.Device.Name
	out.DeviceFormFactor = raw.Device.FormFactor
	return out, nil
}

// Roots 返回盘符顶层固定根（8 个）。
func (c *Client) Roots(ctx context.Context) ([]Entry, error) {
	body, err := c.getJSON(ctx, pathRoots, nil)
	if err != nil {
		return nil, err
	}
	var resp rootsResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode roots: %w", err)
	}
	return resp.Roots, nil
}

// List 枚举目录；id 为 stor:primary 或 p:<base64url(path)>。
// 返回条目与服务端 revision（W2 取子项最大 modified_ms）。
// 注意：W2 不支持翻页（next_cursor 恒空），不要发 cursor 参数。
func (c *Client) List(ctx context.Context, id string) ([]Entry, int64, error) {
	body, err := c.getJSON(ctx, pathList, url.Values{"id": {id}})
	if err != nil {
		return nil, 0, err
	}
	var resp listResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, 0, fmt.Errorf("decode list: %w", err)
	}
	// W2 不支持翻页（传 cursor 直接 400）；若有朝一日服务端回了游标，
	// M1 桥只会呈现第一页——必须在日志里暴露而不是静默截断。
	if resp.NextCursor != "" {
		c.log.Warnf("list id=%s returned next_cursor=%q; M1 bridge shows first page only", id, resp.NextCursor)
	}
	return resp.Entries, resp.Revision, nil
}

// StatFS 取卷容量。
func (c *Client) StatFS(ctx context.Context) (FSInfo, error) {
	body, err := c.getJSON(ctx, pathStatfs, nil)
	if err != nil {
		return FSInfo{}, err
	}
	var resp statfsResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return FSInfo{}, fmt.Errorf("decode statfs: %w", err)
	}
	if resp.BlockSize <= 0 {
		resp.BlockSize = 4096
	}
	return FSInfo{TotalBytes: resp.TotalBytes, FreeBytes: resp.FreeBytes, BlockSize: resp.BlockSize}, nil
}

var contentRangeRe = regexp.MustCompile(`bytes (\d+)-(\d+)/(\d+|\*)`)

// RangeResult 是一次 Range 读的结果。
type RangeResult struct {
	Data     []byte
	First    int64 // 服务端 Content-Range 实际起点
	Last     int64 // 实际末点（含）
	Total    int64 // 文件总大小；未知为 -1
	FullBody bool  // 服务端无视 Range 回了 200 全量（Data 为按窗口截取后的切片）
}

// ContentRange 读 [start, end]（闭区间，单位字节）。
//
// 行为：
//   - 206：按 Content-Range 校验实际窗口与 total，返回正文；
//   - 200：服务端忽略 Range（实测 W2 不会，防御）——丢弃 start 之前字节、
//     只保留窗口长度，避免大文件整包进内存；
//   - 416：返回 *APIError（HTTPStatus=416），调用方按 EOF 处理；
//   - 传输层错误/5xx/retriable：退避重试一次。
func (c *Client) ContentRange(ctx context.Context, id string, start, end int64) (RangeResult, error) {
	if start < 0 || end < start {
		return RangeResult{}, fmt.Errorf("bad range %d-%d", start, end)
	}
	want := end - start + 1
	var out RangeResult
	out.Total = -1

	doOnce := func(timeout time.Duration) (*http.Response, []byte, error) {
		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
			c.baseURL+pathContent+"?id="+url.QueryEscape(id), nil)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		resp, err := c.httpc.Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer resp.Body.Close()
		// 206 正文 == want；防御 200 全量回包时也只读 end+1 字节（我们的块 ≤1MiB，
		// 所以无论文件多大都不会整包进内存）。
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, end+1))
		if rerr != nil {
			return resp, nil, rerr
		}
		return resp, data, nil
	}

	resp, data, err := c.retryCall(ctx, func() (*http.Response, []byte, error) {
		return doOnce(contentTimeout)
	})
	if err != nil {
		return out, err
	}
	switch {
	case resp.StatusCode == http.StatusPartialContent:
		m := contentRangeRe.FindStringSubmatch(resp.Header.Get("Content-Range"))
		if m == nil {
			return out, fmt.Errorf("206 without valid Content-Range: %q", resp.Header.Get("Content-Range"))
		}
		out.First, _ = strconv.ParseInt(m[1], 10, 64)
		out.Last, _ = strconv.ParseInt(m[2], 10, 64)
		if m[3] != "*" {
			out.Total, _ = strconv.ParseInt(m[3], 10, 64)
		}
		if int64(len(data)) != out.Last-out.First+1 {
			return out, fmt.Errorf("206 body %d bytes != range %d-%d", len(data), out.First, out.Last)
		}
		out.Data = data
		return out, nil
	case resp.StatusCode == http.StatusOK:
		// Range 被忽略：跳过前 start 字节，只取 want 长度，绝不保留整包。
		fullLen := int64(-1)
		if cl := resp.Header.Get("Content-Length"); cl != "" {
			fullLen, _ = strconv.ParseInt(cl, 10, 64)
		}
		buf := bytes.NewReader(data)
		if fullLen >= 0 && start >= fullLen {
			return out, &APIError{HTTPStatus: http.StatusRequestedRangeNotSatisfiable, Code: "RANGE_NOT_SATISFIED", Message: "start past EOF (200 path)"}
		}
		if _, err := io.CopyN(io.Discard, buf, start); err != nil {
			// data 被 LimitReader 限长且 start 已超出实际可读窗口 → 按 EOF/416 语义
			return out, &APIError{HTTPStatus: http.StatusRequestedRangeNotSatisfiable, Code: "RANGE_NOT_SATISFIED", Message: "start past served body"}
		}
		keep := want
		if int64(buf.Len()) < keep {
			keep = int64(buf.Len())
		}
		slice := make([]byte, keep)
		_, _ = io.ReadFull(buf, slice)
		out.First, out.Last, out.FullBody = start, start+keep-1, true
		if fullLen >= 0 {
			out.Total = fullLen
		}
		c.log.Warnf("server ignored Range and returned 200 for id=%s; sliced %d bytes from offset %d", id, keep, start)
		out.Data = slice
		return out, nil
	default:
		return out, parseAPIError(resp.StatusCode, data)
	}
}

// getJSON 发带认证的 GET 并重试，返回原始 JSON。
func (c *Client) getJSON(ctx context.Context, path string, q url.Values) ([]byte, error) {
	u := c.baseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	_, data, err := c.retryCall(ctx, func() (*http.Response, []byte, error) {
		reqCtx, cancel := context.WithTimeout(ctx, metadataTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/json")
		resp, err := c.httpc.Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer resp.Body.Close()
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 元数据 JSON 上限 4MiB
		if rerr != nil {
			return resp, nil, rerr
		}
		return resp, body, nil
	})
	if err != nil {
		return nil, err
	}
	return data, nil
}

// retryCall 执行一次已构造好的 HTTP 调用；传输层错误或 retriable 响应退避重试一次。
// 401/403/404/400/416/501 等确定结果不重试（由 fn 内读取 body 后据状态码判断）。
func (c *Client) retryCall(ctx context.Context, fn func() (*http.Response, []byte, error)) (*http.Response, []byte, error) {
	var lastTransport error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			c.log.Warnf("retrying failed call after %s (transport=%v)", retryBackoff, lastTransport)
			select {
			case <-time.After(retryBackoff):
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		resp, body, err := fn()
		if err != nil {
			lastTransport = err
			continue // 连接断开/reset/超时：重试一次
		}
		if resp.StatusCode < 400 {
			return resp, body, nil
		}
		ae := parseAPIError(resp.StatusCode, body)
		// 仅对服务端自陈 retriable 或网关类 5xx 做一次重试；416/501/401 立即返回。
		if (ae.Retriable || resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504) && attempt == 0 {
			c.log.Warnf("retriable HTTP %d (%s) for %s", resp.StatusCode, ae.Code, resp.Request.URL.Path)
			lastTransport = ae
			continue
		}
		return resp, nil, ae
	}
	return nil, nil, fmt.Errorf("phonefs call failed after retry: %w", lastTransport)
}

func parseAPIError(status int, body []byte) *APIError {
	ae := &APIError{HTTPStatus: status, Code: "HTTP_" + strconv.Itoa(status)}
	var eb errBody
	if json.Unmarshal(body, &eb) == nil && eb.Error.Code != "" {
		ae.Code = eb.Error.Code
		ae.Message = eb.Error.Message
		ae.TraceID = eb.Error.TraceID
		ae.Retriable = eb.Error.Retriable
		ae.Details = eb.Error.Details
		// details.received：UPLOAD_INCOMPLETE 续传水位（JSON number → float64）
		if eb.Error.Details != nil {
			if rv, ok := eb.Error.Details["received"].(float64); ok {
				ae.Received = int64(rv)
			}
		}
	} else {
		ae.Message = strings.TrimSpace(string(body))
	}
	return ae
}
