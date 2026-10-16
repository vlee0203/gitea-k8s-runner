package config

import (
	"os"
	"path/filepath"
	"testing"
)

const testPEM = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"

// 挂载点原样交给它就行 —— 包括 kubelet 那套 ..data/..<时间戳> 记账项，
// 以及"文件其实是符号链接"这件事。
func TestReadCASkipsKubeletBookkeepingAndFollowsSymlinks(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "..2026_10_16_00_00_00.000000000")
	if err := os.Mkdir(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "corp-ca.crt"), []byte(testPEM), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..2026_10_16_00_00_00.000000000", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..data", "corp-ca.crt"), filepath.Join(dir, "corp-ca.crt")); err != nil {
		t.Fatal(err)
	}

	got, err := ReadCA(dir)
	if err != nil {
		t.Fatalf("ReadCA(dir): %v", err)
	}
	if string(got) != testPEM {
		t.Errorf("ReadCA(dir) = %q, want %q", got, testPEM)
	}
}

func TestReadCASingleFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "corp-ca.crt")
	if err := os.WriteFile(f, []byte(testPEM), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadCA(f); err != nil || string(got) != testPEM {
		t.Errorf("ReadCA(file) = %q, %v; want %q, nil", got, err, testPEM)
	}
}

// 路径写错 / 指到非证书文件时必须报错：否则 CA 静默不生效，只在 workflow 某一步炸 TLS。
func TestReadCARejectsMissingAndNonPEM(t *testing.T) {
	if _, err := ReadCA(filepath.Join(t.TempDir(), "nope.crt")); err == nil {
		t.Error("不存在的路径应当报错")
	}
	f := filepath.Join(t.TempDir(), "not-a-cert")
	if err := os.WriteFile(f, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCA(f); err == nil {
		t.Error("非 PEM 文件应当报错")
	}
	if _, err := ReadCA(t.TempDir()); err == nil {
		t.Error("空目录应当报错")
	}
}
