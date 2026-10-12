// Package executor creates and removes the one-shot pods that run a single
// claimed task, together with the per-assignment capability Secret each pod
// authenticates to the facade with.
package executor

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Labels and annotations this controller stamps on the resources it owns.
const (
	ManagedByLabel    = "app.kubernetes.io/managed-by"
	ManagedByValue    = "gitea-k8s-runner"
	AssignmentIDLabel = "gitea-k8s-runner/assignment"
)

// CapabilityKey is the Secret key holding the assignment capability.
const CapabilityKey = "capability"

// Spec describes one executor to launch for a claimed task.
type Spec struct {
	AssignmentID string // Gitea task id; used as the pod/secret name suffix and label
	Image        string
	Labels       []string
	Capability   string // bearer secret the pod presents to the facade
	FacadeURL    string // runner --instance target
	Resources    corev1.ResourceRequirements
	// ActiveDeadlineSeconds, when > 0, bounds how long the pod may run.
	ActiveDeadlineSeconds int64
}

// Manager owns executor pods and capability Secrets in one namespace.
type Manager struct {
	cs        kubernetes.Interface
	namespace string
}

// New returns a Manager operating in namespace.
func New(cs kubernetes.Interface, namespace string) *Manager {
	return &Manager{cs: cs, namespace: namespace}
}

// Start creates the capability Secret then the executor Pod for one assignment.
func (m *Manager) Start(ctx context.Context, s Spec) error {
	name := "gkr-" + s.AssignmentID
	labels := map[string]string{ManagedByLabel: ManagedByValue, AssignmentIDLabel: s.AssignmentID}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.namespace, Labels: labels},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{CapabilityKey: s.Capability},
	}
	if _, err := m.cs.CoreV1().Secrets(m.namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create capability secret: %w", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.namespace, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy:         corev1.RestartPolicyNever,
			ActiveDeadlineSeconds: positiveOrNil(s.ActiveDeadlineSeconds),
			// TODO: set an explicit TTL/GC story; v1 relies on Delete after the task.
			Containers: []corev1.Container{{
				Name:      "runner",
				Image:     s.Image,
				Resources: s.Resources,
				Env: []corev1.EnvVar{
					{Name: "GITEA_INSTANCE_URL", Value: s.FacadeURL},
					{Name: "GITEA_RUNNER_REGISTRATION_TOKEN_FILE", Value: "/etc/gkr/capability"},
					{Name: "GITEA_RUNNER_LABELS", Value: join(s.Labels)},
					{Name: "GITEA_RUNNER_EPHEMERAL", Value: "true"},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "capability", MountPath: "/etc/gkr", ReadOnly: true}},
			}},
			Volumes: []corev1.Volume{{
				Name:         "capability",
				VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: name}},
			}},
		},
	}
	if _, err := m.cs.CoreV1().Pods(m.namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create executor pod: %w", err)
	}
	return nil
}

// List returns the assignment ids of executor pods this controller owns.
func (m *Manager) List(ctx context.Context) ([]string, error) {
	pods, err := m.cs.CoreV1().Pods(m.namespace).List(ctx, metav1.ListOptions{LabelSelector: ManagedByLabel + "=" + ManagedByValue})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(pods.Items))
	for i := range pods.Items {
		if id := pods.Items[i].Labels[AssignmentIDLabel]; id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// Delete removes an assignment's pod and capability Secret. Missing resources
// are not an error.
func (m *Manager) Delete(ctx context.Context, assignmentID string) error {
	name := "gkr-" + assignmentID
	if err := m.cs.CoreV1().Pods(m.namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err := m.cs.CoreV1().Secrets(m.namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func positiveOrNil(n int64) *int64 {
	if n <= 0 {
		return nil
	}
	return &n
}

func join(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}
