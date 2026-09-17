// Package relayclient 消费 model-surge-relay 的调度面（/v1/*，Bearer 调度密钥）。
//
// 调度层负责选目标与记运行态，本服务只问「这次发给谁」并回报结果。
// 客户端鉴权也在调度层：client_key 原样转发过去比对，本服务不判断。
package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	baseURL     string
	dispatchKey string
	http        *http.Client
}

func New(baseURL, dispatchKey string) *Client {
	return &Client{
		baseURL:     strings.TrimRight(baseURL, "/"),
		dispatchKey: dispatchKey,
		http:        &http.Client{Timeout: 30 * time.Second},
	}
}

// Dispatch 问调度层这次该发给哪个上游目标。
//
// 换目标不需要租约：把失败的 model_id 追加进 req.TriedIDs 再调一次即可。
func (c *Client) Dispatch(ctx context.Context, req DispatchRequest) (*DispatchResponse, error) {
	var out DispatchResponse
	if err := c.do(ctx, http.MethodPost, PathDispatch, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Report 上报本次调用结果。ReportID 是幂等键，重放同一份不会重复计数，
// 因此 outbox 重试是安全的。
func (c *Client) Report(ctx context.Context, rep ResultReport) error {
	var out ReportResponse
	return c.do(ctx, http.MethodPost, PathResults, rep, &out)
}

// Models 取用户模型清单（user model 名，不是上游 model_id）。
func (c *Client) Models(ctx context.Context) ([]UserModelSummary, error) {
	var out ModelsResponse
	if err := c.do(ctx, http.MethodGet, PathModels, nil, &out); err != nil {
		return nil, err
	}
	return out.Models, nil
}

// Ready 供健康检查用；探模型清单，可达即为真。
func (c *Client) Ready(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := c.Models(ctx)
	return err == nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return &Error{Code: CodeInternal, Message: fmt.Sprintf("encode request: %v", err)}
		}
		body = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return &Error{Code: CodeInternal, Message: fmt.Sprintf("build request: %v", err)}
	}
	req.Header.Set("Authorization", "Bearer "+c.dispatchKey)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// 调度层不可达算可重试：网络抖动稍后可能自愈。
		// 但不是 target_unavailable —— 那表示候选耗尽，会让重试循环停下。
		return &Error{Code: CodeInternal, Retryable: true,
			Message: fmt.Sprintf("relay unreachable: %v", err)}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return &Error{Code: CodeInternal, Retryable: true,
			Message: fmt.Sprintf("read relay response: %v", err)}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeError(resp.StatusCode, raw)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &Error{Code: CodeInternal, Message: fmt.Sprintf("decode relay response: %v", err)}
	}
	return nil
}

// decodeError 优先信任 relay 的错误信封（含它自己判定的 retryable）；
// 拿不到信封时（反代返回 HTML 之类）按状态码兜底。
func decodeError(status int, raw []byte) *Error {
	var env errorEnvelope
	if err := json.Unmarshal(raw, &env); err == nil && env.Error.Code != "" {
		return &env.Error
	}
	return &Error{
		Code:      codeForStatus(status),
		Message:   fmt.Sprintf("relay returned %d: %s", status, snippet(raw)),
		Retryable: status >= 500 || status == http.StatusServiceUnavailable,
	}
}

func codeForStatus(status int) string {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return CodeUnauthorized
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusBadRequest:
		return CodeInvalidRequest
	case http.StatusServiceUnavailable:
		return CodeTargetUnavailable
	default:
		return CodeInternal
	}
}

func snippet(raw []byte) string {
	const maxLen = 256
	s := strings.TrimSpace(string(raw))
	if len(s) > maxLen {
		return s[:maxLen]
	}
	return s
}
