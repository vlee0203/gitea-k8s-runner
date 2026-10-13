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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	runnerv1 "gitea.dev/actionslib/runner/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/vlee0203/gitea-k8s-runner/internal/config"
	"github.com/vlee0203/gitea-k8s-runner/internal/executor"
	"github.com/vlee0203/gitea-k8s-runner/internal/facade"
	"github.com/vlee0203/gitea-k8s-runner/internal/giteaclient"
)

const (
	pollInterval     = 3 * time.Second
	errorBackoff     = 10 * time.Second
	podWatchInterval = 10 * time.Second
	// reportGrace is how long to wait for a final report after the pod ends
	// before treating the job as unreported.
	reportGrace = 15 * time.Second
)

// Worker owns one runner identity for one pool and up to Pool.Slots in-flight jobs.
type Worker struct {
	cfg    *config.Config
	pool   config.Pool
	client *giteaclient.Client
	facade *facade.Server
	exec   *executor.Manager

	version int64 // last tasks version seen from Gitea
	running atomic.Int64
}

// Running reports how many jobs this worker currently has in flight. Callers
// must not swap the worker's identity while this is non-zero: in-flight reports
// are bound to the runner id that claimed them.
func (w *Worker) Running() int64 { return w.running.Load() }

// New wires a worker for one pool.
func New(cfg *config.Config, pool config.Pool, client *giteaclient.Client, f *facade.Server, exec *executor.Manager) *Worker {
	return &Worker{cfg: cfg, pool: pool, client: client, facade: f, exec: exec}
}

// Run claims and runs tasks until pollCtx is cancelled. In-flight jobs run under
// jobCtx, which the caller keeps alive past pollCtx so they can drain before
// being cancelled.
func (w *Worker) Run(pollCtx, jobCtx context.Context) {
	sem := make(chan struct{}, w.pool.Slots)
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		select {
		case sem <- struct{}{}:
		case <-pollCtx.Done():
			return
		}

		task, version, err := w.client.FetchTask(pollCtx, w.version)
		if err != nil {
			<-sem
			log.Printf("worker %v: fetch task: %v", w.pool.Labels, err)
			if !sleep(pollCtx, errorBackoff) {
				return
			}
			continue
		}
		if task == nil {
			<-sem
			w.version = version
			if !sleep(pollCtx, pollInterval) {
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
			w.run(jobCtx, t)
		}(task)
	}
}

// run hands one claimed task to a fresh executor pod and waits for it to finish.
func (w *Worker) run(ctx context.Context, t *runnerv1.Task) {
	w.running.Add(1)
	defer w.running.Add(-1)
	repo, runID, job, workflow := taskContext(t)
	id := strconv.FormatInt(t.Id, 10)

	if feats := unsupportedWorkflowFeatures(t.GetWorkflowPayload(), job); len(feats) > 0 {
		why := "runner has no docker daemon; unsupported: " + strings.Join(feats, ", ")
		log.Printf("worker %v: job %s (%s): %s", w.pool.Labels, id, job, why)
		w.fail(ctx, t.Id, id, why)
		return
	}

	// 要构建/推送镜像的 job：Pod 里多一个官方 kaniko 镜像的容器（待命），runner 容器里的
	// `docker` 替身把构建交给它（见 container/docker.sh、executor.Spec.KanikoImage）。
	// kaniko 不能和 runner 挤在一个容器里 —— 它会清空自己容器的文件系统。
	kaniko := ""
	if w.cfg.KanikoImage != "" && needsImageBuild(t.GetWorkflowPayload(), job) {
		kaniko = w.cfg.KanikoImage
		log.Printf("worker %v: job %s (%s) builds an image → Pod 里加 kaniko 容器", w.pool.Labels, id, job)
	}

	capability := randomCapability()
	a := facade.NewAssignment(t, capability, w.client)
	w.facade.Add(a)
	defer w.facade.Remove(capability)

	if err := w.exec.Start(ctx, executor.Spec{
		AssignmentID:          id,
		Image:                 w.pool.Image,
		Labels:                w.pool.Labels,
		Capability:            capability,
		FacadeURL:             w.cfg.FacadeURL,
		Resources:             resources(w.pool.Resources),
		ImagePullSecret:       w.cfg.ImagePullSecret,
		RunnerConfig:          w.cfg.CacheConfigYAML(),
		InjectRunner:          w.pool.InjectRunner,
		KanikoImage:           kaniko,
		RunnerImage:           w.cfg.RunnerImage,
		ActiveDeadlineSeconds: timeoutSeconds(w.pool.Timeout),
		Repo:                  repo,
		Job:                   job,
		RunID:                 runID,
		Workflow:              workflow,
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

	// Wait for the executor to report a terminal state. A pod that dies without
	// reporting (OOMKilled, evicted) would otherwise leave the slot occupied and
	// the task stuck until Gitea's own timeout, so watch the pod too.
	watch := time.NewTicker(podWatchInterval)
	defer watch.Stop()
	for {
		select {
		case <-a.Done():
			return
		case <-ctx.Done():
			w.fail(ctx, t.Id, id, "cut short by shutdown")
			return
		case <-watch.C:
			phase, err := w.exec.Phase(ctx, id)
			if err != nil {
				log.Printf("worker %v: watch executor %s: %v", w.pool.Labels, id, err)
				continue
			}
			if phase == corev1.PodSucceeded || phase == corev1.PodFailed || phase == "" {
				// Give the last report a moment to arrive before deciding it never will.
				select {
				case <-a.Done():
				case <-time.After(reportGrace):
					w.fail(ctx, t.Id, id, "executor ended without reporting a result")
				}
				return
			}
		}
	}
}

// unsupportedWorkflowFeatures reports workflow keys this runner cannot honour.
// The executor pod has no Docker daemon, so `container:` fails and `services:`
// is silently ignored (act starts no service, yet the job "succeeds" and fails
// later with a confusing connection error). Reject those up front with a reason.
func unsupportedWorkflowFeatures(payload []byte, jobKey string) []string {
	if jobKey == "" {
		return nil
	}
	var wf struct {
		Jobs map[string]struct {
			Container any `yaml:"container"`
			Services  any `yaml:"services"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(payload, &wf); err != nil {
		return nil
	}
	job, ok := wf.Jobs[jobKey]
	if !ok {
		return nil
	}
	var out []string
	if job.Container != nil {
		out = append(out, "container:")
	}
	if job.Services != nil {
		out = append(out, "services:")
	}
	return out
}

// needsImageBuild reports whether the job builds or pushes a container image,
// either with the ordinary `docker build` / `docker push` / `docker login`, with
// the official docker/* actions, or with kaniko directly. Those jobs have to run
// in a container that can build (see the BuildImage field), so that a workflow
// written for GitHub runs unchanged on runs-on: ubuntu-latest.
func needsImageBuild(payload []byte, jobKey string) bool {
	if jobKey == "" {
		return false
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Uses string `yaml:"uses"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(payload, &wf); err != nil {
		return false
	}
	job, ok := wf.Jobs[jobKey]
	if !ok {
		return false
	}
	for _, step := range job.Steps {
		if strings.HasPrefix(step.Uses, "docker/") {
			return true
		}
		for _, marker := range []string{"docker build", "docker buildx", "docker push", "docker login", "kaniko/executor"} {
			if strings.Contains(step.Run, marker) {
				return true
			}
		}
	}
	return false
}

// taskContext reads the (flat) context Gitea attached to the task; it is used
// only for the executor pod's name, annotations and reported runner name.
// Gitea's context is flat — keys are top level, not nested under "github".
func taskContext(t *runnerv1.Task) (repo, runID, job, workflow string) {
	f := t.GetContext().GetFields()
	return f["repository"].GetStringValue(), f["run_id"].GetStringValue(),
		f["job"].GetStringValue(), f["workflow"].GetStringValue()
}

// fail marks an already-claimed task as failed and records why in the job log.
// A task that never reached a pod has no other log output, so without this the
// job would fail with an empty log and no explanation.
func (w *Worker) fail(ctx context.Context, taskID int64, id, why string) {
	ctx = context.WithoutCancel(ctx)
	rows := []*runnerv1.LogRow{{Time: timestamppb.Now(), Content: "##[error] " + why}}
	if _, err := w.client.UpdateLog(ctx, taskID, 0, rows, true); err != nil {
		log.Printf("worker %v: write failure log for job %s: %v", w.pool.Labels, id, err)
	}
	if _, err := w.client.UpdateTask(ctx, &runnerv1.TaskState{Id: taskID, Result: runnerv1.Result_RESULT_FAILURE}, nil); err != nil {
		log.Printf("worker %v: fail job %s (%s): %v", w.pool.Labels, id, why, err)
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
	// Ephemeral storage must be set explicitly wherever a LimitRange imposes a
	// small default (e.g. 100Mi), or a checkout would fill it.
	if r.EphemeralStorage != "" {
		if q, err := resource.ParseQuantity(r.EphemeralStorage); err == nil {
			limits[corev1.ResourceEphemeralStorage] = q
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
