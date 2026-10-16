package executor

import (
	"context"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	runnerv1 "gitea.dev/actionslib/runner/v1"
)

// 非 injectRunner 的池跑的是**镜像自带的 run.sh**（上游的），它注册时读
// `GITEA_INSTANCE_URL`（`--instance "${GITEA_INSTANCE_URL}"`）。把它改名成我们的
// CONTROLLER_URL 之后，这类池的 runner 一直卡在
// `Invalid input … error="instance address is empty"` 的无限重试里：job 永远不开始、
// 一条日志都没有、Pod 也不退出（上游 run.sh 默认无限次重试）——实测踩过，所以两个名字都钉住。
func TestPodCarriesBothTheStockAndOurInstanceEnv(t *testing.T) {
	cs := fake.NewSimpleClientset()
	m := New(cs, "ns", nil)
	const url = "http://gitea-k8s-runner:8080"

	if err := m.Start(context.Background(), Spec{
		AssignmentID: "1", Image: "runner:test", Capability: "cap-1",
		ControllerURL: url, Task: &runnerv1.Task{Id: 1}, PoolKey: "pool",
		Labels: []string{"self-hosted", "ubuntu-latest"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pods, err := cs.CoreV1().Pods("ns").List(context.Background(), metav1.ListOptions{})
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("pods = %d (%v), want 1", len(pods.Items), err)
	}
	var runner *corev1.Container
	for i := range pods.Items[0].Spec.Containers {
		if pods.Items[0].Spec.Containers[i].Name == runnerContainer {
			runner = &pods.Items[0].Spec.Containers[i]
		}
	}
	if runner == nil {
		t.Fatal("没有 runner 容器")
	}
	got := map[string]string{}
	for _, e := range runner.Env {
		got[e.Name] = e.Value
	}
	for _, name := range []string{"GITEA_INSTANCE_URL", "CONTROLLER_URL"} {
		if got[name] != url {
			t.Errorf("runner 容器 %s = %q, want %q", name, got[name], url)
		}
	}
	// 注册重试必须有上界：上游默认无限重试，等于把"注册不上"变成"永远卡着、没有日志"。
	if n, err := strconv.Atoi(got["GITEA_MAX_REG_ATTEMPTS"]); err != nil || n <= 0 {
		t.Errorf("GITEA_MAX_REG_ATTEMPTS = %q, 必须是个正整数（0/空 = 上游的无限重试）", got["GITEA_MAX_REG_ATTEMPTS"])
	}
}
