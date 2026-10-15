package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/vlee0203/gitea-k8s-runner/internal/config"
	"github.com/vlee0203/gitea-k8s-runner/internal/facade"
)

// activityResolver reports each capability's quiet time from a table the test controls.
type activityResolver struct {
	mu   sync.Mutex
	idle map[string]time.Duration
}

func (r *activityResolver) Resolve(context.Context, string) (*facade.Assignment, error) {
	return nil, errors.New("not used in this test")
}

func (r *activityResolver) IdleFor(_ context.Context, capability string) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.idle[capability]
}

func (r *activityResolver) MarkReported(context.Context, string) error { return nil }
func (r *activityResolver) Reported(context.Context, string) bool      { return false }

// drain 期间只掐"不再说话"的 job：还在上报的必须留着跑完，从没观察到活动的也不能动
// （宁可多等，不能砍掉还在干活的 job）。两个承诺靠这一个判断同时成立，所以要钉住。
func TestCancelSilentJobsCancelsOnlyObservedSilentOnes(t *testing.T) {
	res := &activityResolver{idle: map[string]time.Duration{
		"cap-silent":  20 * time.Minute, // 静默太久 → 卡死
		"cap-active":  2 * time.Second,  // 刚说过话 → 留着
		"cap-unknown": 0,                // 没观察到 → 不动
	}}
	w := New(&config.Config{}, config.Pool{Labels: []string{"self-hosted"}}, nil, facade.New(res), nil)

	cancelled := map[string]bool{}
	w.trackInFlight("1", func() { cancelled["1"] = true }, "cap-silent")
	w.trackInFlight("2", func() { cancelled["2"] = true }, "cap-active")
	w.trackInFlight("3", func() { cancelled["3"] = true }, "cap-unknown")

	if n := w.CancelSilentJobs(context.Background(), 10*time.Minute); n != 1 {
		t.Fatalf("CancelSilentJobs = %d, want 1（只该掐静默的那个）", n)
	}
	if !cancelled["1"] {
		t.Error("静默的 job 没有被取消")
	}
	if cancelled["2"] {
		t.Error("还在上报的 job 被取消了 —— 违反了「发布不砍在跑 job」的承诺")
	}
	if cancelled["3"] {
		t.Error("没有观察到活动的 job 被取消了 —— 没人观察不等于它卡住")
	}
}
