package worker

import (
	"reflect"
	"testing"
)

func TestUnsupportedWorkflowFeatures(t *testing.T) {
	payload := []byte(`
name: w
jobs:
  plain:
    runs-on: ubuntu-latest
    steps: [{run: echo hi}]
  withservices:
    runs-on: ubuntu-latest
    services:
      redis: {image: redis:alpine}
    steps: [{run: echo hi}]
  withcontainer:
    runs-on: ubuntu-latest
    container: node:20-alpine
    steps: [{run: node -v}]
`)

	cases := []struct {
		job  string
		want []string
	}{
		{"plain", nil},
		{"withservices", []string{"services:"}},
		{"withcontainer", []string{"container:"}},
		{"missing", nil},
		{"", nil},
	}
	for _, tc := range cases {
		if got := unsupportedWorkflowFeatures(payload, tc.job); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("job %q: got %v, want %v", tc.job, got, tc.want)
		}
	}
}
