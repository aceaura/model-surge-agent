// Package relayclient 消费 model-surge-relay 的调度面（/v1/*，Bearer 调度密钥）。
//
// 调度层负责选目标与记运行态，本服务只问「这次发给谁」并回报结果。
// 客户端鉴权也在调度层：client_key 原样转发过去比对，本服务不判断。
package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aceaura/model-surge-agent/backend/textsafe"
)

type Client struct {
	baseURL     string
	dispatchKey string
	http        *http.Client
}

// New 用内置默认的传输参数构造客户端。
func New(baseURL, dispatchKey string) *Client {
	return NewWithOptions(baseURL, dispatchKey, Options{})
}

// NewWithOptions 构造客户端，传输层按 opts 配置（零值取内置默认）。
func NewWithOptions(baseURL, dispatchKey string, opts Options) *Client {
	return &Client{
		baseURL:     strings.TrimRight(baseURL, "/"),
		dispatchKey: dispatchKey,
		http: &http.Client{
			Transport: newTransport(opts),
			Timeout:   durationOrDefault(opts.Timeout, defaultTimeout),
			// 控制面对端是自家服务，任何 3xx 都是故障而不是一次寻址。
			CheckRedirect: refuseRedirect,
		},
	}
}

// refuseRedirect 拒绝控制面的任何重定向。
//
// 必须显式拒绝。默认策略下本机探针实测：一个 302 会把带 body 的
// POST /v1/dispatch 改写成无体 GET 打向重定向目标，同 host 时 Authorization
// 原样带过去，而调用方拿到的是 200 与目标伪造的那份调度结果——整条链上没有
// 任何暴露面。跨 host 时凭据虽被删（实测 auth 为空），请求仍然成功并采纳了
// 对方的响应体。
//
// 返回错误而不是 http.ErrUseLastResponse：后者会把 3xx 交给 do，于是
// decodeError 要读正文拼 snippet，而 3xx 的正文常常就是那个重定向目标 URL，
// 其 query 里可能带密钥。直接报错则连正文都不读。
func refuseRedirect(req *http.Request, via []*http.Request) error {
	return errRedirect
}

// errRedirect 的文本里刻意不带任何 URL，且 do 里要用它自己的文本而不是包装后的
// 错误：标准库包装时会把重定向目标的完整 URL（含 query）打进去。
var errRedirect = errors.New("relay returned a redirect; control plane does not follow")

// maxResponseBytes 是控制面响应体的读取上限。
//
// 小于数据面的 32MiB：控制面最大的响应是 /v1/models 的清单，每项五个短字段，
// 8MiB 能装十万级条目，比任何真实清单宽几个数量级。上限只为防失控，
// 控制面没有「合理的大响应」这一形态，所以不做成可配。
const maxResponseBytes = 8 << 20

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
		// 重定向单独归因，且不能拼 err：标准库把这个错误包进 *url.Error 时
		// 打的是**重定向之后**那个 URL（实测「Get "http://host/v1/models
		// ?leaked_key=…": relay returned a redirect」），于是目标的 query
		// 会顺着错误消息进日志与流水。
		//
		// 也不可重试：3xx 是对端配置或有人在中间插了一跳，再试一次还是同一个。
		if errors.Is(err, errRedirect) {
			return &Error{Code: CodeInternal, Message: errRedirect.Error()}
		}
		// 调度层不可达算可重试：网络抖动稍后可能自愈。
		// 但不是 target_unavailable —— 那表示候选耗尽，会让重试循环停下。
		return &Error{Code: CodeInternal, Retryable: true,
			Message: fmt.Sprintf("relay unreachable: %v", err)}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return &Error{Code: CodeInternal, Retryable: true,
			Message: fmt.Sprintf("read relay response: %v", err)}
	}
	// 超限的判定必须在状态码分支之前：否则一个 500 带着失控的大体会先进
	// decodeError，运维看到的是「relay returned 500: <html>…」的 256 字节
	// snippet，而真正的异常——响应体失控——没有任何暴露面。
	//
	// 对端行为异常，重试不会让它变小，所以不可重试。
	if len(raw) > maxResponseBytes {
		return &Error{Code: CodeInternal,
			Message: fmt.Sprintf("relay response exceeds the %d byte limit", maxResponseBytes)}
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
	return textsafe.Truncate(strings.TrimSpace(string(raw)), maxLen)
}
