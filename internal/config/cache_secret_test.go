package config

import (
	"os"
	"path/filepath"
	"testing"
)

func minimalEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GITEA_INSTANCE_URL", "https://gitea.example.com")
	t.Setenv("CONTROLLER_URL", "http://gitea-k8s-runner:8080")
	pools := filepath.Join(t.TempDir(), "pools.yaml")
	body := "pools:\n  - labels: [self-hosted]\n    image: example/runner:1\n    slots: 1\n"
	if err := os.WriteFile(pools, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POOLS_FILE", pools)
}

// controller 用自己的一行 env 拿缓存密钥（与 cache-server 怎么配置无关）。
func TestCacheServerSecretFromEnv(t *testing.T) {
	minimalEnv(t)
	t.Setenv("CACHE_SERVER_SECRET", "s3cret")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.CacheServerSecret != "s3cret" {
		t.Fatalf("CacheServerSecret = %q, want %q", c.CacheServerSecret, "s3cret")
	}
}
