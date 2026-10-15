package facade

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	runnerv1 "gitea.dev/actionslib/runner/v1"
)

// fakeResolver stands in for the k8s-backed resolver: the RPC surface is what these
// tests cover, and K8sResolver has its own round-trip test.
type fakeResolver struct {
	mu       sync.Mutex
	byCap    map[string]*Assignment
	seen     map[string]time.Time
	reported map[string]bool
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{byCap: map[string]*Assignment{}, seen: map[string]time.Time{}, reported: map[string]bool{}}
}

func (f *fakeResolver) add(a *Assignment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byCap[a.Capability] = a
}

func (f *fakeResolver) Resolve(_ context.Context, capability string) (*Assignment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.byCap[capability]
	if a == nil {
		return nil, errors.New("unknown capability")
	}
	f.seen[capability] = time.Now()
	return a, nil
}

func (f *fakeResolver) IdleFor(_ context.Context, capability string) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	at, ok := f.seen[capability]
	if !ok {
		return 0
	}
	return time.Since(at)
}

func (f *fakeResolver) MarkReported(_ context.Context, capability string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reported[capability] = true
	return nil
}

func (f *fakeResolver) Reported(_ context.Context, capability string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reported[capability]
}

// recordingUpstream stands in for the Gitea client.
type recordingUpstream struct {
	taskID int64
	result runnerv1.Result
	logs   int
}

func (u *recordingUpstream) UpdateTask(_ context.Context, state *runnerv1.TaskState, _ map[string]string) (*runnerv1.TaskState, error) {
	if state != nil {
		u.taskID, u.result = state.Id, state.Result
	}
	return state, nil
}

func (u *recordingUpstream) UpdateLog(_ context.Context, taskID, index int64, rows []*runnerv1.LogRow, _ bool) (int64, error) {
	u.logs += len(rows)
	return int64(len(rows)), nil
}

// 执行 Pod 的完整握手：注册 → 取任务（恰好一次）→ 上报终态。任何实例都能做这些，
// 因为服务端只依赖 resolver，不依赖本进程记得什么。
func TestServesAnAssignmentItDidNotStart(t *testing.T) {
	res := newFakeResolver()
	up := &recordingUpstream{}
	res.add(NewAssignment(&runnerv1.Task{Id: 7}, "cap-1", up))
	s := New(res)

	if _, err := s.Register(context.Background(), connect.NewRequest(&runnerv1.RegisterRequest{Token: "cap-1"})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	ctx := context.Background()
	fetch := connect.NewRequest(&runnerv1.FetchTaskRequest{})
	fetch.Header().Set(tokenHeader, "cap-1")
	got, err := s.FetchTask(ctx, fetch)
	if err != nil {
		t.Fatalf("FetchTask: %v", err)
	}
	if got.Msg.Task == nil || got.Msg.Task.Id != 7 {
		t.Fatalf("FetchTask task = %v, want id 7", got.Msg.Task)
	}

	upReq := connect.NewRequest(&runnerv1.UpdateTaskRequest{State: &runnerv1.TaskState{Id: 7, Result: runnerv1.Result_RESULT_SUCCESS}})
	upReq.Header().Set(tokenHeader, "cap-1")
	if _, err := s.UpdateTask(ctx, upReq); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	if up.result != runnerv1.Result_RESULT_SUCCESS {
		t.Errorf("upstream saw result %v, want SUCCESS", up.result)
	}
	if !s.Reported(ctx, "cap-1") {
		t.Error("终态上报后 Reported 应为 true（worker 靠它区分「跑完了」和「没上报就死了」）")
	}
}

// 不认识的 capability 一律 401：facade 是入口，不能对任何人开口。
func TestUnknownCapabilityIsUnauthenticated(t *testing.T) {
	s := New(newFakeResolver())
	req := connect.NewRequest(&runnerv1.FetchTaskRequest{})
	req.Header().Set(tokenHeader, "nope")
	if _, err := s.FetchTask(context.Background(), req); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("FetchTask err = %v, want unauthenticated", err)
	}
}
