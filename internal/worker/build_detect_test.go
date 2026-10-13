package worker

import "testing"

func TestNeedsImageBuild(t *testing.T) {
	cases := []struct {
		name string
		wf   string
		job  string
		want bool
	}{
		{"docker run 命令", `
jobs:
  b:
    runs-on: ubuntu-latest
    steps:
      - run: docker build -t x .
      - run: docker push x
`, "b", true},
		{"官方 action", `
jobs:
  b:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: docker/build-push-action@v6
        with: { push: true }
`, "b", true},
		{"kaniko 写法", `
jobs:
  b:
    steps:
      - run: /kaniko/executor --context=dir://. --destination=x
`, "b", true},
		{"普通 job 不应命中", `
jobs:
  t:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: go test ./...
`, "t", false},
		{"只构建另一个 job", `
jobs:
  no: { runs-on: ubuntu-latest, steps: [ { run: echo hi } ] }
  yes: { runs-on: ubuntu-latest, steps: [ { run: docker build -t x . } ] }
`, "no", false},
		{"job 名对不上", `
jobs:
  b: { runs-on: ubuntu-latest, steps: [ { run: docker build . } ] }
`, "other", false},
	}
	for _, c := range cases {
		if got := needsImageBuild([]byte(c.wf), c.job); got != c.want {
			t.Errorf("%s: needsImageBuild = %v, want %v", c.name, got, c.want)
		}
	}
}
