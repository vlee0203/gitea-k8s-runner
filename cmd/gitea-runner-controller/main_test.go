package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// 执行 Pod 用 facade 的地址当"Gitea"，所以落在 facade 上的请求，Host 是 facade 的服务名。
// 转发给真 Gitea 时必须改写成目标主机，否则按 Host 路由的网关会回 404；而 Director 与
// Rewrite 只能有一个，两者都设会让每个请求都变成 502。这两个坑都真出过，所以钉成测试。
func TestGiteaProxyRewritesHostAndKeepsPath(t *testing.T) {
	var gotHost, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotPath = r.Host, r.URL.Path
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	facade := httptest.NewServer(newGiteaProxy(target))
	defer facade.Close()

	req, err := http.NewRequest(http.MethodGet, facade.URL+"/volcano/ci-test.git/info/refs", nil)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟 runner：请求发给 facade，Host 头带的是 facade 自己的名字
	req.Host = "gitea-k8s-runner:8080"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200（502 通常是 Director 与 Rewrite 同时设了）", resp.StatusCode)
	}
	if gotHost != target.Host {
		t.Errorf("上游收到的 Host = %q, want %q（说明没有改写 Host，网关按 Host 路由时会 404）", gotHost, target.Host)
	}
	if want := "/volcano/ci-test.git/info/refs"; gotPath != want {
		t.Errorf("上游收到的 path = %q, want %q", gotPath, want)
	}
}
