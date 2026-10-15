package worker

import (
	"testing"
	"time"

	runnerv1 "gitea.dev/actionslib/runner/v1"

	"github.com/vlee0203/gitea-k8s-runner/internal/config"
	"github.com/vlee0203/gitea-k8s-runner/internal/facade"
)

// drain 期间只掐"不再说话"的 job：还在上报的必须留着跑完。
// 这条约束是设计承诺（发布不砍在跑 job），而卡死的 job 又不能堵满 SHUTDOWN_TIMEOUT，
// 两者同时成立全靠这个判断，所以要钉住。
func TestCancelSilentJobsCancelsOnlySilentOnes(t *testing.T) {
	w := New(&config.Config{}, config.Pool{Labels: []string{"self-hosted"}}, nil, nil, nil)

	cancelled := map[string]bool{}
	silent := facade.NewAssignment(&runnerv1.Task{Id: 1}, "cap-silent", nil)
	w.trackInFlight("1", func() { cancelled["1"] = true }, silent)
	active := facade.NewAssignment(&runnerv1.Task{Id: 2}, "cap-active", nil)
	w.trackInFlight("2", func() { cancelled["2"] = true }, active)

	time.Sleep(30 * time.Millisecond)
	active.Touch() // 这个 job 刚跟 facade 说过话

	if n := w.CancelSilentJobs(20 * time.Millisecond); n != 1 {
		t.Fatalf("CancelSilentJobs = %d, want 1（只该掐静默的那个）", n)
	}
	if !cancelled["1"] {
		t.Error("静默的 job 没有被取消")
	}
	if cancelled["2"] {
		t.Error("还在上报的 job 被取消了 —— 违反了「发布不砍在跑 job」的承诺")
	}
}
