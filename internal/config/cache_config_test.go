package config

import (
	"os"
	"path/filepath"
	"testing"
)

func minimalEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GITEA_INSTANCE_URL", "https://gitea.example.com")
	pools := filepath.Join(t.TempDir(), "pools.yaml")
	body := "pools:\n  - labels: [self-hosted]\n    image: example/runner:1\n    slots: 1\n"
	if err := os.WriteFile(pools, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POOLS_FILE", pools)
}

func writeTemp(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// 密钥直接写在配置里（值只此一处，cache-server 读的是同一个文件）。
func TestCacheSecretFromConfig(t *testing.T) {
	minimalEnv(t)
	t.Setenv("CACHE_SERVER_CONFIG_FILE", writeTemp(t, "config.yaml", "cache:\n  external_secret: s3cret\n"))

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.CacheServerSecret != "s3cret" {
		t.Fatalf("CacheServerSecret = %q, want %q", c.CacheServerSecret, "s3cret")
	}
}

// 上游另一种写法：配置里给路径，值在另一个文件里。
func TestCacheSecretFromConfigFilePointer(t *testing.T) {
	minimalEnv(t)
	secret := writeTemp(t, "secret", "from-file\n")
	t.Setenv("CACHE_SERVER_CONFIG_FILE", writeTemp(t, "config.yaml",
		"cache:\n  external_secret_file: "+secret+"\n"))

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.CacheServerSecret != "from-file" {
		t.Fatalf("CacheServerSecret = %q, want the trimmed file value", c.CacheServerSecret)
	}
}
