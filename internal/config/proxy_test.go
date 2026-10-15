package config

import (
	"strings"
	"testing"
)

func TestProxyEnv(t *testing.T) {
	c := &Config{
		GiteaURL:       "https://gitea.example.com",
		ControllerURL:  "http://gitea-k8s-runner:8080",
		CacheServerURL: "http://gitea-runner-cache:8088/",
	}
	// 没有代理时：不带代理变量，但仍给出集群内 NO_PROXY 列表（替身转交 workflow 自带代理时要用）
	bare := c.ProxyEnv()
	if _, ok := bare["HTTP_PROXY"]; ok {
		t.Fatalf("no proxy configured → want no HTTP_PROXY, got %v", bare)
	}
	if !strings.Contains(bare["GKR_PLATFORM_NO_PROXY"], ".svc.cluster.local") {
		t.Fatalf("GKR_PLATFORM_NO_PROXY missing in-cluster entries: %v", bare)
	}

	t.Setenv("HTTPS_PROXY", "http://proxy.corp:3128")
	t.Setenv("NO_PROXY", "example.org")
	got := c.ProxyEnv()
	if got["HTTPS_PROXY"] != "http://proxy.corp:3128" || got["https_proxy"] != "http://proxy.corp:3128" {
		t.Errorf("HTTPS_PROXY not passed through: %v", got)
	}
	if got["NODE_USE_ENV_PROXY"] != "1" {
		t.Errorf("NODE_USE_ENV_PROXY missing (node actions would ignore the proxy): %v", got)
	}
	np := got["NO_PROXY"]
	if np != got["no_proxy"] {
		t.Errorf("NO_PROXY/no_proxy differ: %q vs %q", np, got["no_proxy"])
	}
	for _, want := range []string{"example.org", "gitea.example.com", "gitea-k8s-runner", "gitea-runner-cache", "localhost", ".svc.cluster.local"} {
		if !strings.Contains(np, want) {
			t.Errorf("NO_PROXY missing %q: %s", want, np)
		}
	}
	if strings.Count(np, "localhost") != 1 {
		t.Errorf("NO_PROXY has duplicates: %s", np)
	}
}
