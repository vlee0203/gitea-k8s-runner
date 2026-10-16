// Package lifecycle reconciles controller-owned executor pods after a restart.
// An assignment's facts (its task, its pool, its capability) live in its Secret,
// so a successor instance can serve a job it did not start instead of failing it:
// recovery means adopting the leftovers, not deleting them. That is the same shape
// as ARC's per-runner Secret plus a reconciler.
package lifecycle

import (
	"context"
	"log"

	"github.com/vlee0203/gitea-runner-controller/internal/executor"
)

// Adopter is a pool's worker: it takes over tracking for an adopted assignment.
type Adopter interface {
	Adopt(id, capability string)
}

// Reconciler reconciles owned executor pods against their assignments.
type Reconciler struct {
	exec     *executor.Manager
	adopters map[string]Adopter // pool key -> the worker that can serve it
}

// New returns a Reconciler over the executor manager and the pool workers.
func New(exec *executor.Manager, adopters map[string]Adopter) *Reconciler {
	return &Reconciler{exec: exec, adopters: adopters}
}

// Recover runs once at startup. Every leftover pod whose assignment Secret is
// present is adopted (any instance can serve it, so nothing needs to be reported
// as failed); a pod with no assignment — or one whose pool this instance does not
// serve — is deleted, so garbage never outlives its facts.
func (r *Reconciler) Recover(ctx context.Context) {
	ids, err := r.exec.List(ctx)
	if err != nil {
		log.Printf("lifecycle: list owned pods: %v", err)
		return
	}
	adopted, dropped := 0, 0
	for _, id := range ids {
		capability, poolKey, ok := r.exec.Assignment(ctx, id)
		if ok {
			if w := r.adopters[poolKey]; w != nil {
				w.Adopt(id, capability)
				adopted++
				continue
			}
		}
		if err := r.exec.Delete(ctx, id); err != nil {
			log.Printf("lifecycle: delete orphan executor %s: %v", id, err)
			continue
		}
		dropped++
	}
	if adopted > 0 || dropped > 0 {
		log.Printf("lifecycle: adopted %d leftover executor(s), dropped %d orphan(s)", adopted, dropped)
	}
}
