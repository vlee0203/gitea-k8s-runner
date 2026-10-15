// Package executor creates and removes the one-shot pods that run a single
// claimed task, together with the per-assignment capability Secret each pod
// authenticates to the facade with.
package executor

import (
	"context"
	"fmt"
	"log"
	"sort"
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

// Secret keys: the assignment capability, and an optional runner config file.
const (
	CapabilityKey = "capability"
	ConfigKey     = "config.yaml"
)

// namePrefix starts every executor pod/secret name.
const namePrefix = "gitea-runner"

// maxNameLen is the DNS label limit.
const maxNameLen = 63

// Spec describes one executor to launch for a claimed task.
type Spec struct {
	AssignmentID  string // Gitea task id: immutable, never reused
	Image         string
	Labels        []string
	Capability    string // bearer secret the pod presents to the facade
	ControllerURL string // 执行 Pod 用来连本 controller 的地址（runner 的 --instance 目标）
	Resources     corev1.ResourceRequirements
	// ImagePullSecret, when set, is used to pull the executor image.
	ImagePullSecret string
	// RunnerConfig, when set, is written as the runner's config.yaml so the job
	// uses shared infrastructure (e.g. the actions cache server).
	RunnerConfig string
	// InjectRunner copies the runner binary from RunnerImage into the pod before
	// it starts. Needed when Image is not a runner image — e.g. the official
	// kaniko image, which must run as its own image to build, so the runner
	// cannot be baked into it.
	InjectRunner bool
	RunnerImage  string
	// KanikoImage, when set, makes this a *build* pod: a container runs the
	// official kaniko image (idle, looping) and the container above shares a
	// scratch directory with it. The `docker` shim in the runner image drops
	// build requests there (see container/docker.sh). kaniko cannot live in the
	// runner's container: it unpacks base images over its own filesystem, which
	// would destroy the runner mid-job — kaniko's own README says it must run as
	// its own image.
	KanikoImage string
	// ProxyEnv, when non-empty, is set on both the runner and the kaniko
	// container: a job pod reaches in-cluster addresses (Gitea, facade, cache)
	// and the internet (actions, base images, package registries), and in a
	// cluster that needs a proxy for egress the in-cluster ones must bypass it
	// (config.Config.ProxyEnv builds that NO_PROXY).
	ProxyEnv map[string]string
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

// injectedRunnerVolume is where the copied runner binary and its entrypoint live.
const injectedRunnerVolume = "runner"

// injectedEntrypoint is a POSIX-sh stand-in for the runner image's run.sh: it
// registers against this controller (CONTROLLER_URL) and runs exactly one job.
// It must stay POSIX because images the runner is injected into (kaniko) ship
// busybox sh, not bash.
const injectedEntrypoint = `#!/bin/sh
set -e
CFG=""
if [ -n "$CONFIG_FILE" ]; then CFG="--config=$CONFIG_FILE"; fi
# The injected-into image may have neither /tmp nor $HOME; the runner volume
# always exists and is writable. cd before registering: register writes .runner
# into the working directory and daemon reads it back from the same place.
cd /opt/gkr
TOKEN=$(cat /etc/gkr/` + CapabilityKey + `)
/opt/gkr/gitea-runner register --instance "$CONTROLLER_URL" --token "$TOKEN" \
  --name "$GITEA_RUNNER_NAME" --labels "$GITEA_RUNNER_LABELS" --ephemeral --no-interactive $CFG
# docker 替身放进 PATH（injectRunner 池的镜像里没有它）
[ -x /opt/gkr/docker ] && cp /opt/gkr/docker /usr/local/bin/docker 2>/dev/null || true
exec /opt/gkr/gitea-runner daemon --once --labels "$GITEA_RUNNER_LABELS" $CFG
`

// injectRunnerScript copies the binary out of the runner image and writes the
// entrypoint above next to it.
var injectRunnerScript = "cp /usr/local/bin/gitea-runner /opt/gkr/gitea-runner" +
	" && chmod +x /opt/gkr/gitea-runner" +
	// 顺带把 `docker` 替身也带过去：injectRunner 的池子主容器不是 runner 镜像，没有它
	" && { cp /usr/local/bin/docker /opt/gkr/docker 2>/dev/null && chmod +x /opt/gkr/docker || true; }" +
	" && cat > /opt/gkr/entrypoint.sh <<'GKR_ENTRYPOINT'\n" + injectedEntrypoint + "GKR_ENTRYPOINT\n" +
	// The chmod must start its own line: a line cannot begin with "&&" after a
	// heredoc, which is a shell syntax error (that failed the init container and
	// killed the pod before the runner ever started).
	"chmod +x /opt/gkr/entrypoint.sh"

const (
	// runnerContainer is the container whose exit decides the job's outcome. A
	// build pod also runs a kaniko container, which stays up until the pod is
	// deleted, so the pod phase alone would never become Succeeded/Failed.
	runnerContainer = "runner"
	// sharedDir is where a build pod's runner container and kaniko container meet.
	sharedDir = "/shared"
)

// kanikoSidecarScript is the loop the kaniko container runs. kaniko must run in
// its own image (its README says so, and for good reason: it unpacks base images
// over the container's filesystem), so the build is not run by the runner but
// handed to this container through the shared directory.
const kanikoSidecarScript = `set -u
shared=` + sharedDir + `
mkdir -p "$shared/req"
while true; do
  for d in "$shared"/req/*/; do
    [ -f "$d/go" ] || continue
    [ -f "$d/done" ] && continue
    set --
    while IFS= read -r a; do set -- "$@" "$a"; done < "$d/args"
    # 凭据放在 kaniko 的默认位置（本版本没有 --docker-config 参数）
    mkdir -p /kaniko/.docker
    [ -f "$shared/kaniko/.docker/config.json" ] && cp "$shared/kaniko/.docker/config.json" /kaniko/.docker/config.json
    # 本次构建的代理设置（替身从 runner 容器转交过来，含 workflow 自带的代理）
    if [ -f "$d/env" ]; then set -a; . "$d/env"; set +a; fi
    /kaniko/executor "$@" > "$d/log" 2>&1
    echo $? > "$d/rc"
    : > "$d/done"
  done
  sleep 1
done`

// proxyEnvVars renders the proxy environment as sorted EnvVars (sorted so a pod
// spec is reproducible).
func proxyEnvVars(m map[string]string) []corev1.EnvVar {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]corev1.EnvVar, 0, len(keys))
	for _, k := range keys {
		out = append(out, corev1.EnvVar{Name: k, Value: m[k]})
	}
	return out
}

// sidecarContainers returns the optional extra containers of a pod.
func sidecarContainers(c *corev1.Container) []corev1.Container {
	if c == nil {
		return nil
	}
	return []corev1.Container{*c}
}

// Start creates the capability Secret then the executor Pod for one assignment.
func (m *Manager) Start(ctx context.Context, s Spec) error {
	name := PodName(s)
	labels := labelsFor(s)
	annotations := annotationsFor(s)

	data := map[string]string{CapabilityKey: s.Capability}
	if s.RunnerConfig != "" {
		data[ConfigKey] = s.RunnerConfig
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.namespace, Labels: labels, Annotations: annotations},
		Type:       corev1.SecretTypeOpaque,
		StringData: data,
	}
	if _, err := m.cs.CoreV1().Secrets(m.namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create capability secret: %w", err)
	}

	env := []corev1.EnvVar{
		{Name: "CONTROLLER_URL", Value: s.ControllerURL},
		{Name: "GITEA_RUNNER_REGISTRATION_TOKEN_FILE", Value: "/etc/gkr/" + CapabilityKey},
		{Name: "GITEA_RUNNER_LABELS", Value: strings.Join(s.Labels, ",")},
		{Name: "GITEA_RUNNER_NAME", Value: runnerName(s)},
		{Name: "GITEA_RUNNER_EPHEMERAL", Value: "true"},
	}
	if s.RunnerConfig != "" {
		env = append(env, corev1.EnvVar{Name: "CONFIG_FILE", Value: "/etc/gkr/" + ConfigKey})
	}

	env = append(env, proxyEnvVars(s.ProxyEnv)...)
	runner := corev1.Container{
		Name:  runnerContainer,
		Image: s.Image,
		// 池镜像用的 tag 是可变的（例如 runner:ubuntu-latest），IfNotPresent 会让 Pod
		// 一直用节点上缓存的旧镜像 —— 换镜像后行为不变会非常难查（实测踩过：新加的
		// `docker` 替身没生效，Pod 里跑的还是真 docker）。
		ImagePullPolicy: corev1.PullAlways,
		Resources:       s.Resources,
		Env:             env,
		VolumeMounts:    []corev1.VolumeMount{{Name: "capability", MountPath: "/etc/gkr", ReadOnly: true}},
	}
	// 构建型 job：runner 与 kaniko 容器共享一个目录，构造请求/日志/退出码都走它。
	if s.KanikoImage != "" {
		env = append(env,
			corev1.EnvVar{Name: "GKR_SHARED", Value: sharedDir},
			// HOME 也放这里：runner 的工作目录与 `actions/checkout` 的工作区都落在共享目录里，
			// kaniko 容器才看得到构建上下文。
			corev1.EnvVar{Name: "HOME", Value: sharedDir + "/home"},
		)
		runner.Env = env
		runner.VolumeMounts = append(runner.VolumeMounts, corev1.VolumeMount{Name: "shared", MountPath: sharedDir})
	}
	volumes := []corev1.Volume{{
		Name:         "capability",
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: name}},
	}}
	var initContainers []corev1.Container
	var sidecar *corev1.Container
	if s.InjectRunner {
		// The job image (e.g. kaniko) has no runner in it, so copy one in and
		// start it with a POSIX-sh entrypoint that image can execute. HOME is
		// pointed at the runner volume too: such images often lack /root and
		// /tmp, and the runner writes state under both.
		runner.Command = []string{"/opt/gkr/entrypoint.sh"}
		runner.Env = append(runner.Env, corev1.EnvVar{Name: "HOME", Value: "/opt/gkr"})
		runner.VolumeMounts = append(runner.VolumeMounts,
			corev1.VolumeMount{Name: injectedRunnerVolume, MountPath: "/opt/gkr"})
		volumes = append(volumes, corev1.Volume{
			Name:         injectedRunnerVolume,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
		initContainers = []corev1.Container{{
			Name:            "runner-binary",
			Image:           s.RunnerImage,
			ImagePullPolicy: corev1.PullAlways,
			Command:         []string{"sh", "-c", injectRunnerScript},
			VolumeMounts:    []corev1.VolumeMount{{Name: injectedRunnerVolume, MountPath: "/opt/gkr"}},
		}}
	}
	if s.KanikoImage != "" {
		shared := corev1.VolumeMount{Name: "shared", MountPath: sharedDir}
		volumes = append(volumes, corev1.Volume{
			Name:         "shared",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
		// 先把目录建好（HOME 要能被 runner 立刻写入），再让 kaniko 容器待命。
		initContainers = append(initContainers, corev1.Container{
			Name:            "shared-dir",
			Image:           s.KanikoImage,
			ImagePullPolicy: corev1.PullAlways,
			Command:         []string{"sh", "-c", "mkdir -p " + sharedDir + "/home " + sharedDir + "/req " + sharedDir + "/kaniko/.docker"},
			VolumeMounts:    []corev1.VolumeMount{shared},
		})
		sidecar = &corev1.Container{
			Name:  "kaniko",
			Image: s.KanikoImage,
			// 构建容器必须带上池的资源：不设就落到命名空间 LimitRange 的默认值
			// （默认 LimitRange 是 100Mi 临时存储），kaniko 解包基础镜像+打快照会立刻超限被驱逐 ——
			// 表现为"构建跑到一半 pod 被 Evicted、job 被取消"（实测踩过）。
			Resources:       s.Resources,
			Env:             proxyEnvVars(s.ProxyEnv), // kaniko 拉推镜像也要走代理
			ImagePullPolicy: corev1.PullAlways,
			Command:         []string{"sh", "-c", kanikoSidecarScript},
			VolumeMounts:    []corev1.VolumeMount{shared},
		}
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.namespace, Labels: labels, Annotations: annotations},
		Spec: corev1.PodSpec{
			RestartPolicy:         corev1.RestartPolicyNever,
			ActiveDeadlineSeconds: positiveOrNil(s.ActiveDeadlineSeconds),
			ImagePullSecrets:      pullSecrets(s.ImagePullSecret),
			InitContainers:        initContainers,
			Containers:            append([]corev1.Container{runner}, sidecarContainers(sidecar)...),
			Volumes:               volumes,
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
	// A build pod's kaniko container runs until the pod is deleted, so the pod
	// phase would stay Running long after the job ended. Report the phase of the
	// container that actually runs the job.
	// A pod that cannot start at all — a bad image, an init container that fails,
	// a missing binary — never reaches a terminal phase: kubelet keeps retrying,
	// the pod sits in Pending, the slot stays taken and a rolling update waits on
	// it forever. Report those as failed so the job is failed, the slot is freed
	// and the deployment can proceed.
	if why := stuckReason(pod); why != "" {
		log.Printf("executor %s cannot start: %s", pod.Name, why)
		return corev1.PodFailed, nil
	}

	for i := range pod.Status.ContainerStatuses {
		cs := pod.Status.ContainerStatuses[i]
		if cs.Name != runnerContainer {
			continue
		}
		if cs.State.Terminated != nil {
			if cs.State.Terminated.ExitCode == 0 {
				return corev1.PodSucceeded, nil
			}
			return corev1.PodFailed, nil
		}
		return corev1.PodRunning, nil
	}
	return pod.Status.Phase, nil
}

// StartFailure returns why the pod for this assignment can never start ("" if it
// may still make progress). The worker writes it into the job log, so the reason
// shows up where the user looks, not only in the controller log.
func (m *Manager) StartFailure(ctx context.Context, assignmentID string) string {
	pod, err := m.find(ctx, assignmentID)
	if err != nil || pod == nil {
		return ""
	}
	return stuckReason(pod)
}

// stuckReason reports why a pod can never start, or "" if it may still make
// progress. Only reasons that a retry cannot fix count: they mean the pod spec
// or the image is wrong.
func stuckReason(pod *corev1.Pod) string {
	unfixable := map[string]bool{
		"StartError":                 true, // 命令/入口不存在（例如镜像里没有 sh）
		"CreateContainerConfigError": true, // 引用的 Secret/ConfigMap 缺失
		"CreateContainerError":       true,
		"InvalidImageName":           true,
		"ErrImagePull":               true, // 镜像名/tag 错了；能拉到的会转成 ImagePullBackOff 再成功
		"ImagePullBackOff":           true,
	}
	check := func(cs corev1.ContainerStatus) string {
		if t := cs.State.Terminated; t != nil && t.ExitCode != 0 && t.Reason == "StartError" {
			return "container " + cs.Name + " cannot start: " + t.Message
		}
		if w := cs.State.Waiting; w != nil && unfixable[w.Reason] {
			return "container " + cs.Name + ": " + w.Reason + " " + w.Message
		}
		return ""
	}
	for _, cs := range pod.Status.InitContainerStatuses {
		if why := check(cs); why != "" {
			return "init " + why
		}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if why := check(cs); why != "" {
			return why
		}
	}
	return ""
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
