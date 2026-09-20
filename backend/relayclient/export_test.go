package relayclient

import (
	"net/http"
	"time"
)

// TransportOf 与 ClientTimeoutOf 只在测试里可见（export_test.go 不进生产二进制）。
//
// 用它们而不是把 Client.http 导出：传输层是构造期一次配好的内部细节，
// 导出给生产代码会让调用方能在运行中改掉连接池，那是共享状态的隐蔽写入点。
func TransportOf(c *Client) (*http.Transport, bool) {
	tr, ok := c.http.Transport.(*http.Transport)
	return tr, ok
}

func ClientTimeoutOf(c *Client) time.Duration { return c.http.Timeout }
