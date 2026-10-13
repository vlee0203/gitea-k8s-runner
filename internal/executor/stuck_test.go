package executor

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestStuckReason(t *testing.T) {
	pod := func(init, cont []corev1.ContainerStatus) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: init, ContainerStatuses: cont}}
	}
	wait := func(name, reason string) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: "boom"}}}
	}
	term := func(name string, code int32) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code}}}
	}
	run := func(name string) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
	}

	cases := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{"init StartError（本次踩到的）", pod([]corev1.ContainerStatus{wait("shared-dir", "StartError"), term("shared-dir", 128)}, nil), true},
		{"镜像拉不动", pod(nil, []corev1.ContainerStatus{wait("runner", "ImagePullBackOff")}), true},
		{"配置错", pod(nil, []corev1.ContainerStatus{wait("runner", "CreateContainerConfigError")}), true},
		{"正常跑着", pod([]corev1.ContainerStatus{term("shared-dir", 0)}, []corev1.ContainerStatus{run("runner"), run("kaniko")}), false},
		{"容器在等（如 ContainerCreating）", pod(nil, []corev1.ContainerStatus{wait("runner", "ContainerCreating")}), false},
		{"空 pod", pod(nil, nil), false},
	}
	for _, c := range cases {
		if got := stuckReason(c.pod) != ""; got != c.want {
			t.Errorf("%s: stuckReason=%v, want %v", c.name, got, c.want)
		}
	}
}
