// Package worker runs one Gitea runner identity for a pool. While it has a free
// slot it claims a task with FetchTask and hands that exact task to a fresh
// executor pod, so the task cannot be picked up by any other job.
package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	runnerv1 "gitea.dev/actionslib/runner/v1"

	"github.com/vlee0203/gitea-k8s-runner/internal/config"
	"github.com/vlee0203/gitea-k8s-runner/internal/executor"
	"github.com/vlee0203/gitea-k8s-runner/internal/facade"
	"github.com/vlee0203/gitea-k8s-runner/internal/giteaclient"
)

const (
	pollInterval = 3 * time.Second
	errorBackoff = 10 * time.Second
)

// Worker owns one runner identity for one pool and up to Pool.Slots in-flight jobs.
type Worker struct {
	cfg    *config.Config
	pool   config.Pool
	client *giteaclient.Client
	facade *facade.Server
	exec   *executor.Manager

	version int64 // last tasks version seen from Gitea
}

// New wires a worker for one pool.
func New(cfg *config.Config, pool config.Pool, client *giteaclient.Client, f *facade.Server, exec *executor.Manager) *Worker {
	return &Worker{cfg: cfg, pool: pool, client: client, facade: f, exec: exec}
}

// Run claims and runs tasks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	sem := make(chan struct{}, w.pool.Slots)
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return
		}

		task, version, err := w.client.FetchTask(ctx, w.version)
		if err != nil {
			<-sem
			log.Printf("worker %v: fetch task: %v", w.pool.Labels, err)
			if !sleep(ctx, errorBackoff) {
				return
			}
			continue
		}
		if task == nil {
			<-sem
			w.version = version
			if !sleep(ctx, pollInterval) {
				return
			}
			continue
		}
		// A task was claimed; zero the version so the next poll queries Gitea again.
		w.version = 0

		wg.Add(1)
		go func(t *runnerv1.Task) {
			defer wg.Done()
			defer func() { <-sem }()
			w.run(ctx, t)
		}(task)
	}
}

// run hands one claimed task to a fresh executor pod and waits for it to finish.
func (w *Worker) run(ctx context.Context, t *runnerv1.Task) {
	capability := randomCapability()
	a := facade.NewAssignment(t, capability, w.client)
	w.facade.Add(a)
	defer w.facade.Remove(capability)

	id := strconv.FormatInt(t.Id, 10)
	if err := w.exec.Start(ctx, executor.Spec{
		AssignmentID:          id,
		Image:                 w.pool.Image,
		Labels:                w.pool.Labels,
		Capability:            capability,
		FacadeURL:             w.cfg.FacadeURL,
		Resources:             resources(w.pool.Resources),
		ActiveDeadlineSeconds: timeoutSeconds(w.pool.Timeout),
	}); err != nil {
		log.Printf("worker %v: start executor for task %s: %v", w.pool.Labels, id, err)
		// The task is already claimed by this runner; fail it so Gitea can rerun it.
		if _, ferr := w.client.UpdateTask(ctx, &runnerv1.TaskState{Id: t.Id, Result: runnerv1.Result_RESULT_FAILURE}, nil); ferr != nil {
			log.Printf("worker %v: fail claimed task %s: %v", w.pool.Labels, id, ferr)
		}
		return
	}
	defer func() {
		if err := w.exec.Delete(context.WithoutCancel(ctx), id); err != nil {
			log.Printf("worker %v: delete executor for task %s: %v", w.pool.Labels, id, err)
		}
	}()

	select {
	case <-a.Done():
	case <-ctx.Done():
	}
}

func resources(r config.ResourceLimits) corev1.ResourceRequirements {
	limits := corev1.ResourceList{}
	if r.CPU != "" {
		if q, err := resource.ParseQuantity(r.CPU); err == nil {
			limits[corev1.ResourceCPU] = q
		}
	}
	if r.Memory != "" {
		if q, err := resource.ParseQuantity(r.Memory); err == nil {
			limits[corev1.ResourceMemory] = q
		}
	}
	if len(limits) == 0 {
		return corev1.ResourceRequirements{}
	}
	return corev1.ResourceRequirements{Limits: limits, Requests: limits}
}

func timeoutSeconds(s string) int64 {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0
	}
	return int64(d.Seconds())
}

func randomCapability() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
