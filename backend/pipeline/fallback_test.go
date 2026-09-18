package pipeline

import (
	"net/http"
	"testing"
)

// 没装配 HTTP 时的回落客户端必须带本层配置。
//
// 回落到 http.DefaultClient 会让「装配漏一行」把整层静默降级成旧行为：
// PerHost 只留 2 条空闲连接、响应头等待不设限，而编译器与所有单测都看不见。
func TestFallbackClientCarriesConnectionLayerConfig(t *testing.T) {
	// 走 client() 而不是直接看变量：要守的是取客户端那条路径的行为，
	// 盯着变量看会漏掉「变量定义对了但取的时候没用它」。
	got := (&Pipeline{}).client()
	if got == http.DefaultClient {
		t.Fatal("回落到了 http.DefaultClient，连接层配置会静默失效")
	}
	tr, ok := got.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("回落客户端的 Transport = %T", got.Transport)
	}
	if tr.MaxIdleConnsPerHost <= http.DefaultMaxIdleConnsPerHost {
		t.Errorf("回落客户端 PerHost = %d，没带上本层默认", tr.MaxIdleConnsPerHost)
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Error("回落客户端响应头等待不设限，上游永不回头时会无限卡住")
	}
}

// 装配了就用装配的那个，不能被回落盖掉。
func TestConfiguredClientWins(t *testing.T) {
	mine := &http.Client{}
	p := &Pipeline{HTTP: mine}
	if p.client() != mine {
		t.Error("装配的客户端没被用上")
	}
}
