package facade

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	runnerv1 "gitea.dev/actionslib/runner/v1"
)

func TestHandoff(t *testing.T) {
	up := &fakeUpstream{}
	s := New()
	task := &runnerv1.Task{Id: 42}
	a := NewAssignment(task, "cap-1", up)
	s.Add(a)
	ctx := context.Background()

	// Register with the capability as the registration token succeeds...
	reg, err := s.Register(ctx, connect.NewRequest(&runnerv1.RegisterRequest{Name: "exec", Token: "cap-1"}))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if reg.Msg.Runner.GetToken() != "cap-1" || reg.Msg.Runner.GetUuid() != "cap-1" {
		t.Fatalf("synthetic runner = %+v", reg.Msg.Runner)
	}
	// ...and an unknown capability is rejected.
	if _, err := s.Register(ctx, connect.NewRequest(&runnerv1.RegisterRequest{Name: "exec", Token: "nope"})); err == nil {
		t.Fatal("register with unknown capability should fail")
	}

	// FetchTask hands the pre-claimed task over exactly once.
	first, err := s.FetchTask(ctx, withToken(&runnerv1.FetchTaskRequest{}, "cap-1"))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if first.Msg.Task == nil || first.Msg.Task.Id != 42 {
		t.Fatalf("first fetch task = %+v", first.Msg.Task)
	}
	second, err := s.FetchTask(ctx, withToken(&runnerv1.FetchTaskRequest{}, "cap-1"))
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if second.Msg.Task != nil {
		t.Fatal("task was handed over twice")
	}
	// FetchTask without the capability is rejected.
	if _, err := s.FetchTask(ctx, connect.NewRequest(&runnerv1.FetchTaskRequest{})); err == nil {
		t.Fatal("fetch without capability should fail")
	}

	// A non-terminal update does not close Done; a terminal one does, and both
	// reach the upstream.
	if _, err := s.UpdateTask(ctx, withToken(&runnerv1.UpdateTaskRequest{State: &runnerv1.TaskState{Id: 42}}, "cap-1")); err != nil {
		t.Fatalf("update running: %v", err)
	}
	select {
	case <-a.Done():
		t.Fatal("Done closed before a terminal result")
	default:
	}
	if _, err := s.UpdateTask(ctx, withToken(&runnerv1.UpdateTaskRequest{State: &runnerv1.TaskState{Id: 42, Result: runnerv1.Result_RESULT_SUCCESS}}, "cap-1")); err != nil {
		t.Fatalf("update terminal: %v", err)
	}
	select {
	case <-a.Done():
	default:
		t.Fatal("Done not closed after a terminal result")
	}
	if up.tasks != 2 {
		t.Fatalf("upstream tasks = %d, want 2", up.tasks)
	}
}

func TestUpdateLogForwards(t *testing.T) {
	up := &fakeUpstream{}
	s := New()
	s.Add(NewAssignment(&runnerv1.Task{Id: 7}, "cap-7", up))

	rows := []*runnerv1.LogRow{{Content: "hello"}}
	resp, err := s.UpdateLog(context.Background(), withToken(&runnerv1.UpdateLogRequest{TaskId: 7, Index: 0, Rows: rows, NoMore: true}, "cap-7"))
	if err != nil {
		t.Fatalf("update log: %v", err)
	}
	if resp.Msg.AckIndex != 1 || up.logs != 1 {
		t.Fatalf("ack = %d, upstream logs = %d", resp.Msg.AckIndex, up.logs)
	}
}

type fakeUpstream struct {
	tasks int
	logs  int
}

func (f *fakeUpstream) UpdateTask(_ context.Context, state *runnerv1.TaskState, _ map[string]string) (*runnerv1.TaskState, error) {
	f.tasks++
	return state, nil
}

func (f *fakeUpstream) UpdateLog(_ context.Context, _ int64, _ int64, _ []*runnerv1.LogRow, _ bool) (int64, error) {
	f.logs++
	return 1, nil
}

func withToken[T any](msg *T, token string) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set(tokenHeader, token)
	return req
}
