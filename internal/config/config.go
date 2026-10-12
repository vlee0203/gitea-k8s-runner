// Package config loads the controller configuration from the environment plus
// the pool definition file (expected to be mounted from a ConfigMap).
package config

import (
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"
)

// ResourceLimits mirrors the subset of Kubernetes resource requirements a pool
// may set for its executor pods.
type ResourceLimits struct {
	CPU    string `yaml:"cpu"`
	Memory string `yaml:"memory"`
}

// Pool is one execution-environment template: a label set, the executor image
// and resources, and how many jobs it may run concurrently.
type Pool struct {
	Labels    []string       `yaml:"labels"`
	Image     string         `yaml:"image"`
	Resources ResourceLimits `yaml:"resources"`
	Slots     int            `yaml:"slots"`
	Timeout   string         `yaml:"timeout"`
}

type poolsFile struct {
	Pools []Pool `yaml:"pools"`
}

// Config is the resolved controller configuration.
type Config struct {
	GiteaURL              string // real Gitea base URL
	RegistrationTokenFile string // file holding this worker's Gitea registration token
	FacadeURL             string // URL an executor pod uses to reach the facade
	ListenAddr            string // facade listen address
	Scope                 string // "namespace" or "cluster"
	TargetNamespace       string // where executor pods are created
	WorkerName            string // base name of the registered runner
	StateSecretName       string // Secret persisting this worker's runner identity
	Pools                 []Pool
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Load reads configuration from the environment. Pools come from the file named
// by POOLS_FILE.
func Load() (*Config, error) {
	c := &Config{
		GiteaURL:              strings.TrimRight(env("GITEA_INSTANCE_URL", ""), "/"),
		RegistrationTokenFile: env("REGISTRATION_TOKEN_FILE", "/etc/gitea-k8s-runner/registration-token"),
		FacadeURL:             env("FACADE_URL", ""),
		ListenAddr:            env("LISTEN_ADDR", ":8080"),
		Scope:                 env("SCOPE", "namespace"),
		TargetNamespace:       env("TARGET_NAMESPACE", ""),
		WorkerName:            env("WORKER_NAME", "gitea-k8s-runner"),
		StateSecretName:       env("STATE_SECRET_NAME", "gitea-k8s-runner-state"),
	}
	if c.GiteaURL == "" {
		return nil, fmt.Errorf("GITEA_INSTANCE_URL is required")
	}
	if c.Scope != "namespace" && c.Scope != "cluster" {
		return nil, fmt.Errorf("SCOPE must be namespace or cluster, got %q", c.Scope)
	}

	pf := env("POOLS_FILE", "/etc/gitea-k8s-runner/pools.yaml")
	b, err := os.ReadFile(pf)
	if err != nil {
		return nil, fmt.Errorf("read pools file %s: %w", pf, err)
	}
	var p poolsFile
	if err := yaml.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("parse pools file %s: %w", pf, err)
	}
	if len(p.Pools) == 0 {
		return nil, fmt.Errorf("pools file %s defines no pools", pf)
	}
	for i, pool := range p.Pools {
		if len(pool.Labels) == 0 || pool.Image == "" || pool.Slots <= 0 {
			return nil, fmt.Errorf("pool %d: labels, image and a positive slots are required", i)
		}
	}
	c.Pools = p.Pools
	return c, nil
}
