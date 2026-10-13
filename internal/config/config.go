// Package config loads the controller configuration from the environment plus
// the pool definition file (expected to be mounted from a ConfigMap).
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// ResourceLimits mirrors the subset of Kubernetes resource requirements a pool
// may set for its executor pods.
type ResourceLimits struct {
	CPU              string `yaml:"cpu"`
	Memory           string `yaml:"memory"`
	EphemeralStorage string `yaml:"ephemeralStorage"`
}

// Pool is one execution-environment template: a label set, the executor image
// and resources, and how many jobs it may run concurrently.
type Pool struct {
	Labels    []string       `yaml:"labels"`
	Image     string         `yaml:"image"`
	Resources ResourceLimits `yaml:"resources"`
	Slots     int            `yaml:"slots"`
	Timeout   string         `yaml:"timeout"`
	// InjectRunner copies the runner binary into the pod from RunnerImage. Set
	// it when Image is not a runner image — e.g. the official kaniko image, which
	// must run as its own image to build, so the runner cannot be baked in.
	InjectRunner bool `yaml:"injectRunner"`
	// RegistrationTokenFile overrides the controller-wide REGISTRATION_TOKEN_FILE
	// for this pool. Registration tokens are repo-scoped, so a pool serving a
	// different repository needs its own token to keep least privilege per repo.
	RegistrationTokenFile string `yaml:"registrationTokenFile"`
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
	ImagePullSecret       string // imagePullSecret for private executor images ("" = none)
	// CacheServerURL and CacheServerSecret, when set, point executor jobs at a
	// shared actions cache server. Each executor is a fresh pod, so without one
	// every job starts with a cold cache.
	CacheServerURL    string
	CacheServerSecret string
	// RunnerImage provides the gitea-runner binary for pools that inject it.
	RunnerImage string
	// BuildImage, when set, is the image used for jobs that build or push a
	// container image (detected from the workflow). Such a job cannot run in the
	// general pool's container: this platform builds with kaniko, which unpacks a
	// base image over its own container filesystem, so it needs a container built
	// for that. The image must be self-contained (runner + kaniko + a `docker`
	// shim); container/build.Dockerfile builds one.
	BuildImage string
	// RegistrationTokenSecret names a k8s Secret (key "token") holding the
	// registration token. When set, the controller reads it through the API
	// instead of a mounted file, so no registration token is written to the pod
	// filesystem and rotation needs no restart.
	RegistrationTokenSecret string
	// ShutdownTimeout is how long in-flight jobs may drain before they are
	// cancelled. Must not exceed the pod's terminationGracePeriodSeconds.
	ShutdownTimeout time.Duration
	Pools           []Pool
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
		GiteaURL:                strings.TrimRight(env("GITEA_INSTANCE_URL", ""), "/"),
		RegistrationTokenFile:   env("REGISTRATION_TOKEN_FILE", "/etc/gitea-k8s-runner/registration-token"),
		FacadeURL:               env("FACADE_URL", ""),
		ListenAddr:              env("LISTEN_ADDR", ":8080"),
		Scope:                   env("SCOPE", "namespace"),
		TargetNamespace:         env("TARGET_NAMESPACE", ""),
		WorkerName:              env("WORKER_NAME", "gitea-k8s-runner"),
		StateSecretName:         env("STATE_SECRET_NAME", "gitea-k8s-runner-state"),
		ImagePullSecret:         env("IMAGE_PULL_SECRET", ""),
		CacheServerURL:          env("CACHE_SERVER_URL", ""),
		RunnerImage:             env("RUNNER_IMAGE", ""),
		BuildImage:              env("BUILD_IMAGE", ""),
		RegistrationTokenSecret: env("REGISTRATION_TOKEN_SECRET", ""),
		ShutdownTimeout:         30 * time.Minute,
	}
	if f := env("CACHE_SERVER_SECRET_FILE", ""); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("read cache server secret %s: %w", f, err)
		}
		c.CacheServerSecret = strings.TrimSpace(string(b))
	}
	if v := env("SHUTDOWN_TIMEOUT", ""); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("SHUTDOWN_TIMEOUT: %w", err)
		}
		c.ShutdownTimeout = d
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
		if pool.InjectRunner && c.RunnerImage == "" {
			return nil, fmt.Errorf("pool %d sets injectRunner but RUNNER_IMAGE is not set", i)
		}
	}
	c.Pools = p.Pools
	return c, nil
}

// PoolTokenFile returns the registration token file a pool registers with.
func (c *Config) PoolTokenFile(p Pool) string {
	if p.RegistrationTokenFile != "" {
		return p.RegistrationTokenFile
	}
	return c.RegistrationTokenFile
}

// PoolStateKey is the key a pool's runner identity is persisted under. Pools
// that use the controller-wide token keep the plain label key; pools with their
// own token get a suffix derived from that token file, so two pools with the same
// labels but different repositories keep separate identities.
func (c *Config) PoolStateKey(p Pool) string {
	base := strings.ReplaceAll(strings.Join(p.Labels, "-"), "/", "_")
	if p.RegistrationTokenFile == "" {
		return base
	}
	sum := sha256.Sum256([]byte(c.PoolTokenFile(p)))
	return base + "-" + hex.EncodeToString(sum[:4])
}

// CacheConfigYAML is the runner config fragment that points an executor job at
// the shared cache server, so every one-shot pod reads and writes the same
// actions cache. Empty when no external cache server is configured.
func (c *Config) CacheConfigYAML() string {
	if c.CacheServerURL == "" || c.CacheServerSecret == "" {
		return ""
	}
	return fmt.Sprintf("cache:\n  external_server: %q\n  external_secret: %q\n",
		c.CacheServerURL, c.CacheServerSecret)
}
