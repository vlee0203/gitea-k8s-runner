package executor

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	corev1 "k8s.io/api/core/v1"

	runnerv1 "gitea.dev/actionslib/runner/v1"
)

// 自签 Gitea（推镜像 / 取 action）要求执行 Pod 信任那份 CA，而各工具链认的环境变量不同。
// 这里钉住"每个容器都拿到合并后的 bundle 和那几个变量"——漏一个就会在真机上表现为
// 某个环节 TLS 失败，而那种失败很难从 job 日志看出来。
//
// CA 的来源是 controller 自己读的那个路径（不依赖操作员给 controller 挂的是 Secret 还是
// ConfigMap、key 叫什么名字），因此这里连一个"操作员的 Secret"都不建：CA 应当出现在这条
// assignment 自己的 Secret 里，Pod 也只引用它。
func TestCAIsHandedOverInTheAssignmentsOwnSecret(t *testing.T) {
	ca := "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	cs := fake.NewSimpleClientset()
	m := New(cs, "ns", []byte(ca))

	if err := m.Start(context.Background(), Spec{
		AssignmentID: "1", Image: "runner:test", Capability: "cap-1",
		ControllerURL: "http://gitea-k8s-runner:8080", Task: &runnerv1.Task{Id: 1},
		PoolKey: "pool", KanikoImage: "kaniko:test",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	pods, err := cs.CoreV1().Pods("ns").List(context.Background(), metav1.ListOptions{})
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("pods = %d (%v), want 1", len(pods.Items), err)
	}
	spec := pods.Items[0].Spec
	name := pods.Items[0].Name

	sec, err := cs.CoreV1().Secrets("ns").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("读 assignment Secret %s: %v", name, err)
	}
	if got := string(sec.Data[caCrtKey]); got != ca {
		t.Errorf("assignment Secret 里的 CA = %q, want %q", got, ca)
	}

	var bundle *corev1.Container
	for i := range spec.InitContainers {
		if spec.InitContainers[i].Name == "ca-bundle" {
			bundle = &spec.InitContainers[i]
		}
	}
	if bundle == nil {
		t.Fatal("缺少 ca-bundle init 容器：没有它就没有合并后的信任库")
	}
	if script := strings.Join(bundle.Command, " "); !strings.Contains(script, caSourceDir+"/*") {
		t.Errorf("ca-bundle 没有按目录收集 CA：%s", script)
	}

	var sourceMounted bool
	for _, v := range spec.Volumes {
		if v.Name != caSecretVol || v.Secret == nil {
			continue
		}
		// 挂的必须是这条 assignment 自己的 Secret，而不是操作员给 controller 挂的那个对象。
		if v.Secret.SecretName != name {
			t.Errorf("CA 卷指向 %s，应当指向 assignment 自己的 Secret %s", v.Secret.SecretName, name)
		}
		sourceMounted = true
	}
	if !sourceMounted {
		t.Error("CA 没有挂进 Pod")
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

// 没配 CA 时不该凭空多出容器/卷/环境变量：这是默认路径，绝大多数部署走它。
func TestNoCAConfiguredLeavesThePodAlone(t *testing.T) {
	cs := fake.NewSimpleClientset()
	m := New(cs, "ns", nil)

	if err := m.Start(context.Background(), Spec{
		AssignmentID: "1", Image: "runner:test", Capability: "cap-1",
		Task: &runnerv1.Task{Id: 1}, PoolKey: "pool",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pods, err := cs.CoreV1().Pods("ns").List(context.Background(), metav1.ListOptions{})
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("pods = %d (%v), want 1", len(pods.Items), err)
	}
	spec := pods.Items[0].Spec
	for _, v := range spec.Volumes {
		if v.Name == caSecretVol {
			t.Error("没配 CA，却挂了 CA 卷")
		}
	}
	for _, c := range spec.InitContainers {
		if c.Name == "ca-bundle" {
			t.Error("没配 CA，却加了 ca-bundle init 容器")
		}
	}
	sec, err := cs.CoreV1().Secrets("ns").Get(context.Background(), pods.Items[0].Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sec.Data[caCrtKey]; ok {
		t.Error("没配 CA，assignment Secret 里却有 " + caCrtKey)
	}
}
