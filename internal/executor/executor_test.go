package executor

import (
	"strings"
	"testing"
)

func TestPodName(t *testing.T) {
	cases := []struct {
		name string
		spec Spec
		want string
	}{
		{"id only", Spec{AssignmentID: "16"}, "gitea-runner-16"},
		{"repo and job", Spec{AssignmentID: "16", Repo: "volcano/ci-test", Job: "hello"}, "gitea-runner-volcano-ci-test-hello-16"},
		{"weird characters are slugged", Spec{AssignmentID: "7", Repo: "A/B_c.d", Job: "build:linux"}, "gitea-runner-a-b-c-d-build-linux-7"},
	}
	for _, tc := range cases {
		if got := PodName(tc.spec); got != tc.want {
			t.Errorf("%s: PodName = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestPodNameStaysUniqueAndBounded(t *testing.T) {
	long := Spec{
		AssignmentID: "123456",
		Repo:         "some-org/some-very-long-repository-name-that-keeps-going",
		Job:          strings.Repeat("jobname", 20),
	}
	got := PodName(long)
	if len(got) > maxNameLen {
		t.Fatalf("PodName = %q (%d chars), want <= %d", got, len(got), maxNameLen)
	}
	// The task id must survive truncation: it is what keeps the name unique.
	if !strings.HasSuffix(got, "-123456") {
		t.Fatalf("PodName = %q, want suffix -123456", got)
	}
}
