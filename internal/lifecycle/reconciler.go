// Package lifecycle reconciles controller-owned executor pods after a restart.
// Assignment state lives only in pod labels, so recovery is: list owned pods,
// fail the task each was running (Gitea can then rerun it) and delete the pod.
package lifecycle

import (
	"context"
	"log"
	"strconv"

	runnerv1 "gitea.dev/actionslib/runner/v1"

	"github.com/vlee0203/gitea-k8s-runner/internal/executor"
	"github.com/vlee0203/gitea-k8s-runner/internal/giteaclient"
)

// Reconciler reconciles owned executor pods against their assignments.
type Reconciler struct {
	exec    *executor.Manager
	clients []*giteaclient.Client
}

// New returns a Reconciler over the executor manager and the workers' clients.
func New(exec *executor.Manager, clients []*giteaclient.Client) *Reconciler {
	return &Reconciler{exec: exec, clients: clients}
}

// Recover runs once at startup.
func (r *Reconciler) Recover(ctx context.Context) {
	ids, err := r.exec.List(ctx)
	if err != nil {
		log.Printf("lifecycle: list owned pods: %v", err)
		return
	}
	for _, id := range ids {
		if taskID, err := strconv.ParseInt(id, 10, 64); err == nil {
			r.failTask(ctx, taskID)
		}
		if err := r.exec.Delete(ctx, id); err != nil {
			log.Printf("lifecycle: delete executor %s: %v", id, err)
		}
	}
	if len(ids) > 0 {
		log.Printf("lifecycle: cleaned up %d leftover executor(s)", len(ids))
	}
}

// failTask asks Gitea to fail a leftover task. A task can only be updated by the
// runner that claimed it, so every client is tried until one succeeds.
func (r *Reconciler) failTask(ctx context.Context, taskID int64) {
	state := &runnerv1.TaskState{Id: taskID, Result: runnerv1.Result_RESULT_FAILURE}
	for _, c := range r.clients {
		if _, err := c.UpdateTask(ctx, state, nil); err == nil {
			return
		}
	}
	log.Printf("lifecycle: could not fail leftover task %d via any worker", taskID)
}
