// Package executor creates and removes the one-shot pods that run a single
// claimed task, together with the per-assignment capability Secret each pod
// authenticates to the facade with.
package executor

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Ownership labels and descriptive annotations. Labels are the source of truth
// for finding our resources; annotations carry the human context.
const (
	ManagedByLabel    = "app.kubernetes.io/managed-by"
	ManagedByValue    = "gitea-k8s-runner"
	AssignmentIDLabel = "gitea-k8s-runner/assignment"

	RepoAnnotation     = "gitea-k8s-runner/repository"
	RunIDAnnotation    = "gitea-k8s-runner/run-id"
	JobAnnotation      = "gitea-k8s-runner/job"
	WorkflowAnnotation = "gitea-k8s-runner/workflow"
)

// CapabilityKey is the Secret key holding the assignment capability.
const CapabilityKey = "capability"

// namePrefix starts every executor pod/secret name.
const namePrefix = "gitea-runner"

// maxNameLen is the DNS label limit.
const maxNameLen = 63

// Spec describes one executor to launch for a claimed task.
type Spec struct {
	AssignmentID string // Gitea task id: immutable, never reused
	Image        string
	Labels       []string
	Capability   string // bearer secret the pod presents to the facade
	FacadeURL    string // runner --instance target
	Resources    corev1.ResourceRequirements
	// ImagePullSecret, when set, is used to pull the executor image.
	ImagePullSecret string
	// ActiveDeadlineSeconds, when > 0, bounds how long the pod may run.
	ActiveDeadlineSeconds int64

	// Descriptive context, used only for the pod name, annotations and the
	// runner name the executor reports itself as.
	Repo     string // e.g. volcano/ci-test
	Job      string // e.g. hello
	RunID    string // e.g. 7
	Workflow string // e.g. test.yml
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

// PodName returns the pod/secret name for a spec: a readable, DNS-safe variant
// of the repository and job, suffixed with the immutable task id so it stays
// unique. The id is kept even when the prefix has to be truncated.
func PodName(s Spec) string {
	head := namePrefix
	if r := slug(s.Repo); r != "" {
		head += "-" + r
	}
	if j := slug(s.Job); j != "" {
		head += "-" + j
	}
	tail := "-" + s.AssignmentID
	if len(head)+len(tail) > maxNameLen {
		head = strings.TrimRight(head[:maxNameLen-len(tail)], "-")
	}
	return head + tail
}

// slug lowercases and replaces any run of non-alphanumeric characters with a
// single dash, so a repository path becomes part of a valid pod name.
func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func runnerName(s Spec) string {
	switch {
	case s.Repo != "" && s.Job != "":
		return s.Repo + " " + s.Job
	case s.Repo != "":
		return s.Repo
	default:
		return PodName(s)
	}
}

func labelsFor(s Spec) map[string]string {
	return map[string]string{ManagedByLabel: ManagedByValue, AssignmentIDLabel: s.AssignmentID}
}

func annotationsFor(s Spec) map[string]string {
	a := map[string]string{}
	if s.Repo != "" {
		a[RepoAnnotation] = s.Repo
	}
	if s.RunID != "" {
		a[RunIDAnnotation] = s.RunID
	}
	if s.Job != "" {
		a[JobAnnotation] = s.Job
	}
	if s.Workflow != "" {
		a[WorkflowAnnotation] = s.Workflow
	}
	return a
}

// Start creates the capability Secret then the executor Pod for one assignment.
func (m *Manager) Start(ctx context.Context, s Spec) error {
	name := PodName(s)
	labels := labelsFor(s)
	annotations := annotationsFor(s)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.namespace, Labels: labels, Annotations: annotations},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{CapabilityKey: s.Capability},
	}
	if _, err := m.cs.CoreV1().Secrets(m.namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create capability secret: %w", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.namespace, Labels: labels, Annotations: annotations},
		Spec: corev1.PodSpec{
			RestartPolicy:         corev1.RestartPolicyNever,
			ActiveDeadlineSeconds: positiveOrNil(s.ActiveDeadlineSeconds),
			ImagePullSecrets:      pullSecrets(s.ImagePullSecret),
			Containers: []corev1.Container{{
				Name:      "runner",
				Image:     s.Image,
				Resources: s.Resources,
				Env: []corev1.EnvVar{
					{Name: "GITEA_INSTANCE_URL", Value: s.FacadeURL},
					{Name: "GITEA_RUNNER_REGISTRATION_TOKEN_FILE", Value: "/etc/gkr/capability"},
					{Name: "GITEA_RUNNER_LABELS", Value: strings.Join(s.Labels, ",")},
					{Name: "GITEA_RUNNER_NAME", Value: runnerName(s)},
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

// find returns the executor pod for an assignment, or nil if it is gone.
func (m *Manager) find(ctx context.Context, assignmentID string) (*corev1.Pod, error) {
	sel := ManagedByLabel + "=" + ManagedByValue + "," + AssignmentIDLabel + "=" + assignmentID
	list, err := m.cs.CoreV1().Pods(m.namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, err
	}
	if len(list.Items) == 0 {
		return nil, nil
	}
	return &list.Items[0], nil
}

// Phase returns the executor pod's phase, or "" if the pod no longer exists.
func (m *Manager) Phase(ctx context.Context, assignmentID string) (corev1.PodPhase, error) {
	pod, err := m.find(ctx, assignmentID)
	if err != nil || pod == nil {
		return "", err
	}
	return pod.Status.Phase, nil
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
	sel := ManagedByLabel + "=" + ManagedByValue + "," + AssignmentIDLabel + "=" + assignmentID
	pods, err := m.cs.CoreV1().Pods(m.namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return err
	}
	for i := range pods.Items {
		if err := m.cs.CoreV1().Pods(m.namespace).Delete(ctx, pods.Items[i].Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	secrets, err := m.cs.CoreV1().Secrets(m.namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return err
	}
	for i := range secrets.Items {
		if err := m.cs.CoreV1().Secrets(m.namespace).Delete(ctx, secrets.Items[i].Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func pullSecrets(name string) []corev1.LocalObjectReference {
	if name == "" {
		return nil
	}
	return []corev1.LocalObjectReference{{Name: name}}
}

func positiveOrNil(n int64) *int64 {
	if n <= 0 {
		return nil
	}
	return &n
}
