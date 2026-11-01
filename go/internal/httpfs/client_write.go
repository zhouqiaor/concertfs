// client_write.go PhoneFS HTTPS v1 写端点（M2，SPEC §6.5.6–6.5.9 冻结契约）。
//
//   - POST   /v1/fs/create    201 Attr；409 ALREADY_EXISTS/404/400 INVALID_NAME
//   - PUT    /v1/fs/content   整写原子替换 200 {received,etag}；
//     分块 Content-Range: bytes S-E/T + X-Upload-Id，
//     非末块 complete:false、末块 complete:true+etag；
//     409 UPLOAD_INCOMPLETE 带 received 供续传
//   - PATCH  /v1/fs/meta      仅文件 name/parent_id/modified_ms；目录 501
//   - DELETE /v1/fs           204 幂等；409 NOT_EMPTY/409 PARTIAL
//   - 写后 mtime：X-Set-Mtime 头（ms；0=不带，服务端默认 now）
//
// 幂等性：PUT 整写/分块（同 Upload-Id+START 覆盖）、PATCH、DELETE 均幂等，
// 传输错误可安全重试一次（retryCall）；POST create 不幂等——重试后若第一次
// 实际已成功会得 409 ALREADY_EXISTS，如实上报由调用方判断。
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
	"strconv"
	"time"
)

const (
	pathCreate = "/v1/fs/create"
	pathMeta   = "/v1/fs/meta"
	pathDelete = "/v1/fs"
	// writeTimeout 单次写请求尝试超时（整写/分块含 1MiB body，局域网信道）。
	writeTimeout = 45 * time.Second
)

// createReq 是 §6.5.6 请求体。
type createReq struct {
	Parent string `json:"parent"`
	Name   string `json:"name"`
	IsDir  bool   `json:"is_dir"`
	Mime   string `json:"mime,omitempty"`
}

// putResp 是 §6.5.7 整写/分块公共响应。
type putResp struct {
	Received int64  `json:"received"`
	Complete bool   `json:"complete"`
	ETag     string `json:"etag"`
}

// patchReq 是 §6.5.8 请求体（至少一项；无 size 字段——P2-12 冻结）。
type patchReq struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	ParentID   string `json:"parent_id,omitempty"`
	ModifiedMs *int64 `json:"modified_ms,omitempty"`
}

// Create 建文件/目录，返回 201 Attr。
func (c *Client) Create(ctx context.Context, parent, name string, isDir bool, mime string) (Entry, error) {
	var out Entry
	body, err := json.Marshal(createReq{Parent: parent, Name: name, IsDir: isDir, Mime: mime})
	if err != nil {
		return out, err
	}
	data, err := c.doWrite(ctx, http.MethodPost, pathCreate, nil, body, "application/json", 0, nil)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("decode create attr: %w", err)
	}
	return out, nil
}

// PutContent 整写（原子替换）：body=全部字节。mtimeMs>0 时带 X-Set-Mtime。
// 返回服务端 etag（"<size>-<modified_ms>"）。
func (c *Client) PutContent(ctx context.Context, id string, data []byte, mtimeMs int64) (string, error) {
	q := url.Values{"id": {id}}
	respData, err := c.doWrite(ctx, http.MethodPut, pathContent, q, data, "application/octet-stream", mtimeMs, nil)
	if err != nil {
		return "", err
	}
	var pr putResp
	if err := json.Unmarshal(respData, &pr); err != nil {
		return "", fmt.Errorf("decode put resp: %w", err)
	}
	return pr.ETag, nil
}

// PutChunk 分块上传一块 [start, start+len(data)-1]/total。
// uploadID 同一上传会话恒定（客户端生成），同 id+START 重传覆盖幂等。
// 返回（received, complete, etag）；末块（start+len==total）complete=true 且带 etag。
// 409 UPLOAD_INCOMPLETE 以 *APIError 返回，Received 字段给出续传水位。
func (c *Client) PutChunk(ctx context.Context, id, uploadID string, start, total int64, data []byte, mtimeMs int64) (int64, bool, string, error) {
	if len(data) == 0 {
		return start, false, "", fmt.Errorf("empty chunk at %d", start)
	}
	end := start + int64(len(data)) - 1
	q := url.Values{"id": {id}}
	hdr := map[string]string{
		"Content-Range": fmt.Sprintf("bytes %d-%d/%d", start, end, total),
		"X-Upload-Id":   uploadID,
	}
	respData, err := c.doWrite(ctx, http.MethodPut, pathContent, q, data, "application/octet-stream", mtimeMs, hdr)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Code == "UPLOAD_INCOMPLETE" {
			return ae.Received, false, "", err
		}
		return 0, false, "", err
	}
	var pr putResp
	if err := json.Unmarshal(respData, &pr); err != nil {
		return 0, false, "", fmt.Errorf("decode chunk resp: %w", err)
	}
	return pr.Received, pr.Complete, pr.ETag, nil
}

// PatchMeta 文件改名/移动/改 mtime（至少给一项）。目录 id 的 name/parent_id
// 变更服务端 501 NOT_SUPPORTED（FR-19，M4 前）→ 调用方映射 ENOSYS。
func (c *Client) PatchMeta(ctx context.Context, id string, name, parentID *string, modifiedMs *int64) (Entry, error) {
	var out Entry
	req := patchReq{ID: id, ModifiedMs: modifiedMs}
	if name != nil {
		req.Name = *name
	}
	if parentID != nil {
		req.ParentID = *parentID
	}
	body, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	data, err := c.doWrite(ctx, http.MethodPatch, pathMeta, nil, body, "application/json", 0, nil)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("decode patch attr: %w", err)
	}
	return out, nil
}

// Delete 删文件/目录；不存在按幂等 204（调用方视为成功）。
// 409 NOT_EMPTY（非空目录 recursive=false）、409 PARTIAL（递归删除部分失败，
// Details 带 deleted/failed）原样返回。
func (c *Client) Delete(ctx context.Context, id string, recursive bool) error {
	q := url.Values{"id": {id}, "recursive": {strconv.FormatBool(recursive)}}
	_, err := c.doWrite(ctx, http.MethodDelete, pathDelete, q, nil, "", 0, nil)
	return err
}

// doWrite 写方法公共执行器：JSON/二进制 body、可选 X-Set-Mtime 与附加头，
// 复用 retryCall（传输错误与 retriable 重试一次）。返回响应 body（204 为空）。
func (c *Client) doWrite(ctx context.Context, method, path string, q url.Values,
	body []byte, contentType string, mtimeMs int64, hdr map[string]string) ([]byte, error) {
	u := c.baseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	_, data, err := c.retryCall(ctx, func() (*http.Response, []byte, error) {
		reqCtx, cancel := context.WithTimeout(ctx, writeTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(reqCtx, method, u, bytes.NewReader(body))
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if mtimeMs > 0 {
			req.Header.Set("X-Set-Mtime", strconv.FormatInt(mtimeMs, 10))
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := c.httpc.Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer resp.Body.Close()
		respBody, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if rerr != nil {
			return resp, nil, rerr
		}
		return resp, respBody, nil
	})
	if err != nil {
		return nil, err
	}
	return data, nil
}
