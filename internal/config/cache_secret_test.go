package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Load() 至少需要 Gitea 地址和一个能解析出池的 pools 文件。
func minimalLoadEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GITEA_INSTANCE_URL", "https://gitea.example.com")
	pools := filepath.Join(t.TempDir(), "pools.yaml")
	body := "pools:\n  - labels: [self-hosted]\n    image: example/runner:1\n    slots: 1\n"
	if err := os.WriteFile(pools, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POOLS_FILE", pools)
}

func writeSecretFile(t *testing.T, body string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// CACHE_SERVER_SECRET 直接给值（部署时写成普通 env），文件形式不再需要。
func TestCacheServerSecretFromEnv(t *testing.T) {
	minimalLoadEnv(t)
	t.Setenv("CACHE_SERVER_SECRET", "from-env")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.CacheServerSecret != "from-env" {
		t.Fatalf("CacheServerSecret = %q, want %q", c.CacheServerSecret, "from-env")
	}
}

// 两个都给时以 env 为准（文件是回落路径）。
func TestCacheServerSecretEnvWinsOverFile(t *testing.T) {
	minimalLoadEnv(t)
	t.Setenv("CACHE_SERVER_SECRET", "from-env")
	t.Setenv("CACHE_SERVER_SECRET_FILE", writeSecretFile(t, "from-file\n"))

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.CacheServerSecret != "from-env" {
		t.Fatalf("CacheServerSecret = %q, want the env value to win", c.CacheServerSecret)
	}
}

// 只给文件时仍然可用（挂载密钥的部署方式）。
func TestCacheServerSecretFallsBackToFile(t *testing.T) {
	minimalLoadEnv(t)
	t.Setenv("CACHE_SERVER_SECRET", "")
	t.Setenv("CACHE_SERVER_SECRET_FILE", writeSecretFile(t, "from-file\n"))

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.CacheServerSecret != "from-file" {
		t.Fatalf("CacheServerSecret = %q, want the trimmed file value", c.CacheServerSecret)
	}
}
