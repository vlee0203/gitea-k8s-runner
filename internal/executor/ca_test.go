package executor

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	corev1 "k8s.io/api/core/v1"

	runnerv1 "gitea.dev/actionslib/runner/v1"
)

// 自签 Gitea（推镜像 / 取 action）要求执行 Pod 信任那份 CA，而各工具链认的环境变量不同。
// 这里钉住"每个容器都拿到合并后的 bundle 和那几个变量"——漏一个就会在真机上表现为
// 某个环节 TLS 失败，而那种失败很难从 job 日志看出来。
func TestCASecretIsInjectedIntoEveryContainer(t *testing.T) {
	ca := "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	cs := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "corp-ca", Namespace: "ns"},
		Data:       map[string][]byte{"ca.crt": []byte(ca)},
	})
	m := New(cs, "ns")

	if err := m.Start(context.Background(), Spec{
		AssignmentID: "1", Image: "runner:test", Capability: "cap-1",
		ControllerURL: "http://gitea-k8s-runner:8080", Task: &runnerv1.Task{Id: 1},
		PoolKey: "pool", CASecret: "corp-ca", KanikoImage: "kaniko:test",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	pods, err := cs.CoreV1().Pods("ns").List(context.Background(), metav1.ListOptions{})
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("pods = %d (%v), want 1", len(pods.Items), err)
	}
	spec := pods.Items[0].Spec

	var bundled bool
	for _, ic := range spec.InitContainers {
		if ic.Name == "ca-bundle" {
			bundled = true
		}
	}
	if !bundled {
		t.Error("缺少 ca-bundle init 容器：没有它就没有合并后的信任库")
	}

	var sourceMounted bool
	for _, v := range spec.Volumes {
		if v.Name == caSecretVol && v.Secret != nil && v.Secret.SecretName == "corp-ca" {
			sourceMounted = true
		}
	}
	if !sourceMounted {
		t.Error("CA Secret 没有挂进 Pod")
	}

	wantEnv := []string{"SSL_CERT_FILE", "CURL_CA_BUNDLE", "GIT_SSL_CAINFO", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE"}
	for _, c := range spec.Containers {
		var mounted bool
		for _, mm := range c.VolumeMounts {
			if mm.MountPath == caBundleDir {
				mounted = true
			}
		}
		if !mounted {
			t.Errorf("容器 %s 没有挂载 CA bundle", c.Name)
		}
		got := map[string]string{}
		for _, e := range c.Env {
			got[e.Name] = e.Value
		}
		for _, name := range wantEnv {
			if got[name] != caBundleFile {
				t.Errorf("容器 %s: %s = %q, want %q", c.Name, name, got[name], caBundleFile)
			}
		}
	}
}
