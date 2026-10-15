// Package facade is the RunnerService endpoint that executor pods talk to. A
// worker hands it the task it has already claimed, so the task cannot be picked
// up by any other job; the pod's stock gitea-runner fetches that task here and
// reports back here, and the facade forwards both to Gitea under the claiming
// runner's identity.
package facade

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"
	pingv1 "gitea.dev/actionslib/ping/v1"
	"gitea.dev/actionslib/ping/v1/pingv1connect"
	runnerv1 "gitea.dev/actionslib/runner/v1"
	"gitea.dev/actionslib/runner/v1/runnerv1connect"
)

// tokenHeader carries the executor's capability on every call after Register.
const tokenHeader = "x-runner-token"

// Upstream reports task state and logs to Gitea under the claiming runner.
type Upstream interface {
	UpdateTask(ctx context.Context, state *runnerv1.TaskState, outputs map[string]string) (*runnerv1.TaskState, error)
	UpdateLog(ctx context.Context, taskID, index int64, rows []*runnerv1.LogRow, noMore bool) (int64, error)
}

// Assignment is one claimed task handed to a single executor pod.
type Assignment struct {
	Task       *runnerv1.Task
	Capability string
	Upstream   Upstream

	done     chan struct{}
	finishOn sync.Once
	mu       sync.Mutex
	served   bool
	// lastSeen is when the executor pod last talked to this facade. A draining
	// instance uses it to tell a job that is still working from one that is wedged
	// (see Worker.CancelSilentJobs).
	lastSeen time.Time
}

// NewAssignment builds an assignment with a fresh Done channel.
func NewAssignment(task *runnerv1.Task, capability string, up Upstream) *Assignment {
	return &Assignment{Task: task, Capability: capability, Upstream: up, done: make(chan struct{}), lastSeen: time.Now()}
}

// Touch records executor activity.
func (a *Assignment) Touch() {
	a.mu.Lock()
	a.lastSeen = time.Now()
	a.mu.Unlock()
}

// IdleFor reports how long the executor has been quiet.
func (a *Assignment) IdleFor() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return time.Since(a.lastSeen)
}

// Done is closed once the task reaches a terminal state.
func (a *Assignment) Done() <-chan struct{} { return a.done }

func (a *Assignment) finish() { a.finishOn.Do(func() { close(a.done) }) }

// take returns the pre-claimed task exactly once.
func (a *Assignment) take() *runnerv1.Task {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.served {
		return nil
	}
	a.served = true
	return a.Task
}

// Server implements the Gitea RunnerService handler for executor pods.
type Server struct {
	mu    sync.Mutex
	byCap map[string]*Assignment
}

// New returns an empty facade.
func New() *Server { return &Server{byCap: map[string]*Assignment{}} }

// Add registers an assignment, reachable by its capability.
func (s *Server) Add(a *Assignment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byCap[a.Capability] = a
}

// Remove drops an assignment.
func (s *Server) Remove(capability string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byCap, capability)
}

// Handler returns the connect service path and handler to mount.
func (s *Server) Handler() (string, http.Handler) {
	return runnerv1connect.NewRunnerServiceHandler(s)
}

func (s *Server) byToken(tok string) *Assignment {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byCap[tok]
}

// Register treats the capability as the registration token and returns a
// synthetic runner; nothing is written to Gitea.
func (s *Server) Register(_ context.Context, req *connect.Request[runnerv1.RegisterRequest]) (*connect.Response[runnerv1.RegisterResponse], error) {
	a := s.byToken(req.Msg.Token)
	if a == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errUnknownCapability)
	}
	a.Touch()
	return connect.NewResponse(&runnerv1.RegisterResponse{Runner: &runnerv1.Runner{
		Uuid: a.Capability, Token: a.Capability, Name: req.Msg.Name, Ephemeral: true,
	}}), nil
}

// Declare acknowledges the executor.
func (s *Server) Declare(_ context.Context, req *connect.Request[runnerv1.DeclareRequest]) (*connect.Response[runnerv1.DeclareResponse], error) {
	a := s.byToken(req.Header().Get(tokenHeader))
	if a == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errUnknownCapability)
	}
	a.Touch() // 执行 Pod 说话 = 这个 job 还活着（drain 判卡死用）
	return connect.NewResponse(&runnerv1.DeclareResponse{Runner: &runnerv1.Runner{
		Uuid: a.Capability, Token: a.Capability, Ephemeral: true,
	}}), nil
}

// FetchTask hands the pre-claimed task to the executor exactly once.
func (s *Server) FetchTask(_ context.Context, req *connect.Request[runnerv1.FetchTaskRequest]) (*connect.Response[runnerv1.FetchTaskResponse], error) {
	a := s.byToken(req.Header().Get(tokenHeader))
	if a == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errUnknownCapability)
	}
	a.Touch() // 执行 Pod 说话 = 这个 job 还活着（drain 判卡死用）
	return connect.NewResponse(&runnerv1.FetchTaskResponse{Task: a.take(), TasksVersion: req.Msg.TasksVersion}), nil
}

// UpdateTask forwards a task state to Gitea and closes Done on a terminal result.
func (s *Server) UpdateTask(ctx context.Context, req *connect.Request[runnerv1.UpdateTaskRequest]) (*connect.Response[runnerv1.UpdateTaskResponse], error) {
	a := s.byToken(req.Header().Get(tokenHeader))
	if a == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errUnknownCapability)
	}
	a.Touch() // 执行 Pod 说话 = 这个 job 还活着（drain 判卡死用）
	state, err := a.Upstream.UpdateTask(ctx, req.Msg.State, req.Msg.Outputs)
	if err != nil {
		return nil, err
	}
	if req.Msg.State != nil && req.Msg.State.Result != runnerv1.Result_RESULT_UNSPECIFIED {
		a.finish()
	}
	return connect.NewResponse(&runnerv1.UpdateTaskResponse{State: state}), nil
}

// UpdateLog forwards a batch of log rows to Gitea.
func (s *Server) UpdateLog(ctx context.Context, req *connect.Request[runnerv1.UpdateLogRequest]) (*connect.Response[runnerv1.UpdateLogResponse], error) {
	a := s.byToken(req.Header().Get(tokenHeader))
	if a == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errUnknownCapability)
	}
	a.Touch() // 执行 Pod 说话 = 这个 job 还活着（drain 判卡死用）
	ack, err := a.Upstream.UpdateLog(ctx, req.Msg.TaskId, req.Msg.Index, req.Msg.Rows, req.Msg.NoMore)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&runnerv1.UpdateLogResponse{AckIndex: ack}), nil
}

// PingHandler returns the Ping service handler to mount alongside the
// RunnerService; the runner pings before it registers.
func (s *Server) PingHandler() (string, http.Handler) {
	return pingv1connect.NewPingServiceHandler(s)
}

// Ping echoes the request.
func (s *Server) Ping(_ context.Context, req *connect.Request[pingv1.PingRequest]) (*connect.Response[pingv1.PingResponse], error) {
	return connect.NewResponse(&pingv1.PingResponse{Data: req.Msg.Data}), nil
}

var errUnknownCapability = errors.New("unknown capability")

var (
	_ runnerv1connect.RunnerServiceHandler = (*Server)(nil)
	_ pingv1connect.PingServiceHandler     = (*Server)(nil)
)
