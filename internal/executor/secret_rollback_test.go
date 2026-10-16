package executor

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	runnerv1 "gitea.dev/actionslib/runner/v1"
)

// Pod 建不出来时（配额不够、被准入拒……）不能把 assignment Secret 留在命名空间里：
// 没有 Pod 的它永远读不到、也不会被回收 —— 实测在 ns-admin 撞配额时留下了一个。
func TestFailedPodCreateRollsBackTheAssignmentSecret(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("exceeded quota")
	})
	m := New(cs, "ns", nil)

	err := m.Start(context.Background(), Spec{
		AssignmentID: "7", Image: "runner:test", Capability: "cap-7",
		Task: &runnerv1.Task{Id: 7}, PoolKey: "pool",
	})
	if err == nil {
		t.Fatal("Pod 建不出来时 Start 应当报错")
	}

	secs, lerr := cs.CoreV1().Secrets("ns").List(context.Background(), metav1.ListOptions{})
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(secs.Items) != 0 {
		t.Errorf("留下了 %d 个 assignment Secret（%s），应当回滚", len(secs.Items), secs.Items[0].Name)
	}
}
