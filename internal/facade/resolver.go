package facade

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"sync"
	"time"

	runnerv1 "gitea.dev/actionslib/runner/v1"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/vlee0203/gitea-runner-controller/internal/executor"
)

// Upstreams returns the Gitea client that owns a pool's runner identity. A task
// can only be reported by the runner that claimed it, so the pool key stored with
// the assignment decides who reports.
type Upstreams func(poolKey string) Upstream

// Resolver answers "whose job is this capability?" without relying on process
// memory. Each assignment's facts live in the per-job Secret the executor writes
// (task + pool key + capability), which is the same shape ARC uses for its
// per-runner jitToken Secret — so any instance can serve any pod, a rollout needs
// no hand-off coordination, and a successor can adopt jobs still in flight.
type Resolver interface {
	Resolve(ctx context.Context, capability string) (*Assignment, error)
	// IdleFor reports how long this capability has been quiet, using both this
	// instance's view and the heartbeat kept in the Secret.
	IdleFor(ctx context.Context, capability string) time.Duration
	// MarkReported records a terminal report; Reported says whether one arrived.
	MarkReported(ctx context.Context, capability string) error
	Reported(ctx context.Context, capability string) bool
}

// K8sResolver resolves assignments from their Secrets.
type K8sResolver struct {
	cs        kubernetes.Interface
	namespace string
	upstreams Upstreams

	mu     sync.Mutex
	seen   map[string]time.Time // capability -> last call this instance served
	sealed map[string]time.Time // capability -> last heartbeat written to the Secret
}

// NewK8sResolver returns a resolver over the executor Secrets in namespace.
func NewK8sResolver(cs kubernetes.Interface, namespace string, upstreams Upstreams) *K8sResolver {
	return &K8sResolver{cs: cs, namespace: namespace, upstreams: upstreams,
		seen: map[string]time.Time{}, sealed: map[string]time.Time{}}
}

// heartbeatInterval is how often activity is written back to the assignment
// Secret. The pod may reach a different instance next time, so the drain needs a
// shared view; writing on every log batch would be far too many writes.
const heartbeatInterval = 30 * time.Second

// Resolve finds the assignment a capability belongs to.
func (r *K8sResolver) Resolve(ctx context.Context, capability string) (*Assignment, error) {
	if capability == "" {
		return nil, fmt.Errorf("empty capability")
	}
	r.touch(ctx, capability)

	list, err := r.cs.CoreV1().Secrets(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: executor.CapabilityHashLabel + "=" + executor.CapabilityHash(capability),
	})
	if err != nil {
		return nil, fmt.Errorf("list assignment secrets: %w", err)
	}
	for i := range list.Items {
		sec := &list.Items[i]
		if sec.Data[executor.CapabilityKey] == nil ||
			string(sec.Data[executor.CapabilityKey]) != capability {
			continue // hash collision, or a capability the caller did not present
		}
		task := &runnerv1.Task{}
		if raw := sec.Data[executor.TaskKey]; len(raw) == 0 {
			return nil, fmt.Errorf("assignment %s has no task", sec.Name)
		} else if decoded, derr := base64.StdEncoding.DecodeString(string(raw)); derr != nil {
			return nil, fmt.Errorf("assignment %s: decode task: %w", sec.Name, derr)
		} else if uerr := proto.Unmarshal(decoded, task); uerr != nil {
			return nil, fmt.Errorf("assignment %s: parse task: %w", sec.Name, uerr)
		}
		up := r.upstreams(string(sec.Data[executor.PoolKeyKey]))
		if up == nil {
			return nil, fmt.Errorf("assignment %s: pool %q has no runner identity on this instance",
				sec.Name, string(sec.Data[executor.PoolKeyKey]))
		}
		return NewAssignment(task, capability, up), nil
	}
	return nil, fmt.Errorf("unknown capability")
}

// touch records activity, and periodically persists it so another instance (the
// one draining, or a successor) can tell a working job from a wedged one.
func (r *K8sResolver) touch(ctx context.Context, capability string) {
	now := time.Now()
	r.mu.Lock()
	r.seen[capability] = now
	due := now.Sub(r.sealed[capability]) >= heartbeatInterval
	if due {
		r.sealed[capability] = now
	}
	r.mu.Unlock()
	if !due {
		return
	}
	if err := r.stampSecret(ctx, capability, now); err != nil {
		log.Printf("facade: heartbeat for %s: %v", executor.CapabilityHash(capability), err)
	}
}

// stampSecret writes the heartbeat into the assignment Secret (annotation only —
// the data stays immutable).
func (r *K8sResolver) stampSecret(ctx context.Context, capability string, at time.Time) error {
	list, err := r.cs.CoreV1().Secrets(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: executor.CapabilityHashLabel + "=" + executor.CapabilityHash(capability),
	})
	if err != nil {
		return err
	}
	for i := range list.Items {
		sec := &list.Items[i]
		if string(sec.Data[executor.CapabilityKey]) != capability {
			continue
		}
		if sec.Annotations == nil {
			sec.Annotations = map[string]string{}
		}
		sec.Annotations[LastSeenAnnotation] = at.UTC().Format(time.RFC3339)
		if _, err := r.cs.CoreV1().Secrets(r.namespace).Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
			return err
		}
		return nil
	}
	return nil
}

// LastSeenAnnotation records the last time an instance served this assignment.
const LastSeenAnnotation = "gitea-k8s-runner/last-seen"

// ReportedAnnotation records that the pod reported a terminal result. With the
// assignment state shared, this is how a worker tells "the job finished" from
// "the pod died without reporting", without holding the assignment itself.
const ReportedAnnotation = "gitea-k8s-runner/reported"

// MarkReported stamps a terminal report onto the assignment Secret.
func (r *K8sResolver) MarkReported(ctx context.Context, capability string) error {
	return r.annotate(ctx, capability, ReportedAnnotation, time.Now().UTC().Format(time.RFC3339))
}

// Reported reports whether the pod's terminal result already reached Gitea.
func (r *K8sResolver) Reported(ctx context.Context, capability string) bool {
	list, err := r.cs.CoreV1().Secrets(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: executor.CapabilityHashLabel + "=" + executor.CapabilityHash(capability),
	})
	if err != nil {
		return false
	}
	for i := range list.Items {
		sec := &list.Items[i]
		if string(sec.Data[executor.CapabilityKey]) == capability {
			return sec.Annotations[ReportedAnnotation] != ""
		}
	}
	return false
}

// annotate sets one annotation on the assignment Secret (its data stays immutable).
func (r *K8sResolver) annotate(ctx context.Context, capability, key, value string) error {
	list, err := r.cs.CoreV1().Secrets(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: executor.CapabilityHashLabel + "=" + executor.CapabilityHash(capability),
	})
	if err != nil {
		return err
	}
	for i := range list.Items {
		sec := &list.Items[i]
		if string(sec.Data[executor.CapabilityKey]) != capability {
			continue
		}
		if sec.Annotations == nil {
			sec.Annotations = map[string]string{}
		}
		sec.Annotations[key] = value
		_, err := r.cs.CoreV1().Secrets(r.namespace).Update(ctx, sec, metav1.UpdateOptions{})
		return err
	}
	return nil
}

// IdleFor reports how long the assignment has been quiet: the most recent of this
// instance's own observations and the heartbeat in the Secret.
func (r *K8sResolver) IdleFor(ctx context.Context, capability string) time.Duration {
	r.mu.Lock()
	last := r.seen[capability]
	r.mu.Unlock()

	list, err := r.cs.CoreV1().Secrets(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: executor.CapabilityHashLabel + "=" + executor.CapabilityHash(capability),
	})
	if err == nil {
		for i := range list.Items {
			sec := &list.Items[i]
			if string(sec.Data[executor.CapabilityKey]) != capability {
				continue
			}
			if ts, perr := time.Parse(time.RFC3339, sec.Annotations[LastSeenAnnotation]); perr == nil && ts.After(last) {
				last = ts
			}
		}
	}
	if last.IsZero() {
		return 0 // unknown: never treat an unobserved job as wedged
	}
	return time.Since(last)
}
