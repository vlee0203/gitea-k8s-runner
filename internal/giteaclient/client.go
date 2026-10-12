// Package giteaclient is the worker-side client of the Gitea RunnerService. A
// worker uses it to register itself, declare its labels, claim one task at a
// time (FetchTask) and report task state and logs.
package giteaclient

import (
	"context"
	"net/http"

	"connectrpc.com/connect"
	runnerv1 "gitea.dev/actionslib/runner/v1"
	"gitea.dev/actionslib/runner/v1/runnerv1connect"
)

const (
	uuidHeader  = "x-runner-uuid"
	tokenHeader = "x-runner-token"
)

// Client wraps the generated RunnerService client and attaches this runner's
// identity headers to every call.
type Client struct {
	svc   runnerv1connect.RunnerServiceClient
	uuid  string
	token string
}

// New builds a client for the given Gitea base URL. uuid/token are empty until
// Register succeeds and SetIdentity is called.
func New(giteaURL string, hc connect.HTTPClient) *Client {
	c := &Client{}
	c.svc = runnerv1connect.NewRunnerServiceClient(hc, giteaURL+"/api/actions",
		connect.WithInterceptors(identityHeaders(&c.uuid, &c.token)))
	return c
}

func identityHeaders(uuid, token *string) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if *uuid != "" {
				req.Header().Set(uuidHeader, *uuid)
			}
			if *token != "" {
				req.Header().Set(tokenHeader, *token)
			}
			return next(ctx, req)
		}
	}
}

// SetIdentity stores the runner identity returned by Register.
func (c *Client) SetIdentity(uuid, token string) { c.uuid, c.token = uuid, token }

// Register registers this worker as a runner using a registration token.
func (c *Client) Register(ctx context.Context, name, regToken string, labels []string, ephemeral bool) (*runnerv1.Runner, error) {
	resp, err := c.svc.Register(ctx, connect.NewRequest(&runnerv1.RegisterRequest{
		Name: name, Token: regToken, Labels: labels, Ephemeral: ephemeral,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Runner, nil
}

// Declare announces this worker's labels before it starts fetching tasks.
func (c *Client) Declare(ctx context.Context, labels []string) error {
	_, err := c.svc.Declare(ctx, connect.NewRequest(&runnerv1.DeclareRequest{Labels: labels}))
	return err
}

// FetchTask asks for one waiting task, given the last tasks version observed.
func (c *Client) FetchTask(ctx context.Context, version int64) (*runnerv1.Task, int64, error) {
	resp, err := c.svc.FetchTask(ctx, connect.NewRequest(&runnerv1.FetchTaskRequest{TasksVersion: version}))
	if err != nil {
		return nil, version, err
	}
	return resp.Msg.Task, resp.Msg.TasksVersion, nil
}

// UpdateTask reports the state of a task.
func (c *Client) UpdateTask(ctx context.Context, state *runnerv1.TaskState, outputs map[string]string) (*runnerv1.TaskState, error) {
	resp, err := c.svc.UpdateTask(ctx, connect.NewRequest(&runnerv1.UpdateTaskRequest{State: state, Outputs: outputs}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.State, nil
}

// UpdateLog sends a batch of log rows for a task.
func (c *Client) UpdateLog(ctx context.Context, taskID, index int64, rows []*runnerv1.LogRow, noMore bool) (int64, error) {
	resp, err := c.svc.UpdateLog(ctx, connect.NewRequest(&runnerv1.UpdateLogRequest{
		TaskId: taskID, Index: index, Rows: rows, NoMore: noMore,
	}))
	if err != nil {
		return 0, err
	}
	return resp.Msg.AckIndex, nil
}

var _ connect.HTTPClient = (*http.Client)(nil)
