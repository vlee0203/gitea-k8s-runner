package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReadCA reads the CA material at path: either a PEM file, or a directory of them
// (the mount point of a Secret or ConfigMap, say) whose files are concatenated in
// name order.
//
// Reading a mount point as-is is the point: kubelet publishes the files of a
// mounted object as symlinks alongside its own ..data bookkeeping, so the walk
// skips those entries and follows the symlinks.
func ReadCA(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	var out []byte
	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "..") {
				continue // kubelet 的 ..data / ..<时间戳> 记账项，不是证书
			}
			b, err := os.ReadFile(filepath.Join(path, e.Name()))
			if err != nil {
				return nil, err
			}
			out = append(out, b...)
			if len(out) > 0 && out[len(out)-1] != '\n' {
				out = append(out, '\n')
			}
		}
	} else if out, err = os.ReadFile(path); err != nil {
		return nil, err
	}
	// 指错文件的代价是"静默不生效"（后面表现为某一步 TLS 报错），这里先挡住。
	if !strings.Contains(string(out), "-----BEGIN CERTIFICATE-----") {
		return nil, fmt.Errorf("%s holds no PEM certificate", path)
	}
	return out, nil
}
