// 真实 Android 服务 + Go encoding/json 的 409 details 跨语言缝测试。
// PHONEFS_TOKEN=... go test -v go_details_resume_test.go
package m2test

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	testBase = "https://192.168.43.1:18443"
	testRoot = "/storage/emulated/0/PhoneFS-m2-go-details"
)

type wireClient struct {
	token string
	hc    *http.Client
}

func TestIncompleteReceivedIsGoNumberAndResumeUsesWatermark(t *testing.T) {
	token := strings.TrimSpace(os.Getenv("PHONEFS_TOKEN"))
	if token == "" {
		t.Fatal("PHONEFS_TOKEN is required")
	}
	base := strings.TrimRight(os.Getenv("PHONEFS_BASE_URL"), "/")
	if base == "" {
		base = testBase
	}
	// 真机为 W2 自签演示证书；测试仅校验协议 JSON 类型与内容，不记录 token。
	c := &wireClient{token: token, hc: &http.Client{
		Timeout:   45 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec
	}}
	c.request(t, http.MethodDelete, "/v1/fs?id="+url.QueryEscape(docID(testRoot))+"&recursive=true", nil, nil, 204)
	t.Cleanup(func() {
		c.request(t, http.MethodDelete, "/v1/fs?id="+url.QueryEscape(docID(testRoot))+"&recursive=true", nil, nil, 204)
	})
	c.request(t, http.MethodPost, "/v1/fs/create", []byte(`{"parent":"stor:primary","name":"PhoneFS-m2-go-details","is_dir":true}`),
		jsonHeaders(), 201)
	c.request(t, http.MethodPost, "/v1/fs/create", []byte(`{"parent":"`+docID(testRoot)+`","name":"resume.bin","is_dir":false}`),
		jsonHeaders(), 201)

	payload := make([]byte, 1000)
	for i := range payload {
		payload[i] = byte(i*37 + 11)
	}
	uploadID := fmt.Sprintf("go-details-resume-%d", time.Now().UnixNano())
	putChunk := func(start, end int, want int) []byte {
		return c.request(t, http.MethodPut, "/v1/fs/content?id="+url.QueryEscape(docID(testRoot+"/resume.bin")),
			payload[start:end+1], map[string]string{
				"Content-Range": fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)),
				"X-Upload-Id":   uploadID,
			}, want)
	}
	first := putChunk(0, 99, 200)
	var firstResp struct {
		Received int64 `json:"received"`
		Complete bool  `json:"complete"`
	}
	if err := json.Unmarshal(first, &firstResp); err != nil || firstResp.Received != 100 || firstResp.Complete {
		t.Fatalf("first chunk response=%s err=%v", first, err)
	}

	conflict := putChunk(900, 999, 409)
	var envelope struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(conflict, &envelope); err != nil {
		t.Fatalf("Go encoding/json cannot decode 409: %v body=%s", err, conflict)
	}
	if envelope.Error.Code != "UPLOAD_INCOMPLETE" {
		t.Fatalf("error code=%q body=%s", envelope.Error.Code, conflict)
	}
	watermark, ok := envelope.Error.Details["received"].(float64)
	if !ok {
		t.Fatalf("details.received dynamic type=%T want float64 body=%s", envelope.Error.Details["received"], conflict)
	}
	if watermark != 100 {
		t.Fatalf("details.received=%v want 100 body=%s", watermark, conflict)
	}

	// 按 Go 客户端解析出的水位续传中间缺口；已有的绝对末块仍在服务端会话中。
	completed := putChunk(int(watermark), 899, 200)
	var done struct {
		Received int64 `json:"received"`
		Complete bool  `json:"complete"`
	}
	if err := json.Unmarshal(completed, &done); err != nil || !done.Complete || done.Received != 1000 {
		t.Fatalf("resume response=%s err=%v", completed, err)
	}
	got := c.request(t, http.MethodGet, "/v1/fs/content?id="+url.QueryEscape(docID(testRoot+"/resume.bin")), nil, nil, 200)
	if !bytes.Equal(got, payload) {
		t.Fatalf("readback mismatch got=%d want=%d", len(got), len(payload))
	}
	t.Logf("Go details.received type=float64 value=%v; resumed 100..899; sha256=%x", watermark, sha256.Sum256(got))
}

func docID(path string) string {
	return "p:" + base64.RawURLEncoding.EncodeToString([]byte(path))
}

func jsonHeaders() map[string]string {
	return map[string]string{"Content-Type": "application/json"}
}

func (c *wireClient) request(t *testing.T, method, path string, body []byte, headers map[string]string, want int) []byte {
	t.Helper()
	base := strings.TrimRight(os.Getenv("PHONEFS_BASE_URL"), "/")
	if base == "" {
		base = testBase
	}
	req, err := http.NewRequest(method, base+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != want {
		t.Fatalf("%s %s status=%d want=%d body=%s", method, path, resp.StatusCode, want, data)
	}
	return data
}
