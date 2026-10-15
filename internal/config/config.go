// Package config loads the controller configuration from the environment plus
// the pool definition file (expected to be mounted from a ConfigMap).
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
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
	ControllerURL         string // URL an executor pod uses to reach this controller (its runner-facing endpoint)
	ListenAddr            string // runner-facing endpoint listen address
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
	// KanikoImage is the official kaniko image. Jobs that build or push a
	// container image (detected from the workflow) get it as an extra container
	// in their pod, and the `docker` shim in the runner image hands builds to it.
	// kaniko must run as its own image — it unpacks base images over the
	// container's filesystem — so it cannot share the runner's container.
	KanikoImage string
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
		ControllerURL:           env("CONTROLLER_URL", ""),
		ListenAddr:              env("LISTEN_ADDR", ":8080"),
		Scope:                   env("SCOPE", "namespace"),
		TargetNamespace:         env("TARGET_NAMESPACE", ""),
		WorkerName:              env("WORKER_NAME", "gitea-k8s-runner"),
		StateSecretName:         env("STATE_SECRET_NAME", "gitea-k8s-runner-state"),
		ImagePullSecret:         env("IMAGE_PULL_SECRET", ""),
		CacheServerURL:          env("CACHE_SERVER_URL", ""),
		RunnerImage:             env("RUNNER_IMAGE", ""),
		KanikoImage:             env("KANIKO_IMAGE", "ghcr.io/osscontainertools/kaniko:v1.28.6-debug"),
		RegistrationTokenSecret: env("REGISTRATION_TOKEN_SECRET", ""),
		ShutdownTimeout:         30 * time.Minute,
	}
	// The shared cache secret, injected by the operator. The controller is a separate service
	// from the cache-server, so it takes the value its own way — no dependency on how the
	// cache-server's config file is written.
	c.CacheServerSecret = env("CACHE_SERVER_SECRET", "")
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
	if c.ControllerURL == "" {
		// 执行 Pod 靠这个地址回连 controller 才能注册/取任务；缺了要在启动时就报，而不是等 job 起不来
		return nil, fmt.Errorf("CONTROLLER_URL is required (executor pods use it to reach this controller)")
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

// ProxyEnv reports the proxy environment for executor pods, or nil when no proxy
// is set on the controller. The controller reads these from its own environment,
// so the operator configures them once, on the Deployment.
//
// A job pod talks to three kinds of address: the Gitea it clones from, the
// controller's facade, and the internet (the actions it runs, the base images it
// builds on, the package registries it pulls from). When egress needs a proxy,
// the in-cluster ones must bypass it — sending them through the proxy is the
// classic way to break every job in a proxied cluster — so they are appended to
// NO_PROXY. Node-based actions also need NODE_USE_ENV_PROXY: since Node 20 the
// built-in fetch ignores the env proxy unless asked, and the executor image's
// Node is 24.
func (c *Config) ProxyEnv() map[string]string {
	inCluster := c.noProxyHosts()
	httpProxy := env("HTTP_PROXY", os.Getenv("http_proxy"))
	httpsProxy := env("HTTPS_PROXY", os.Getenv("https_proxy"))
	// GKR_PLATFORM_NO_PROXY is always set: a workflow may bring its own proxy for
	// its steps (workflow/secret env), and the docker shim forwards it to the
	// kaniko container — merging this list in keeps in-cluster traffic direct even
	// when the workflow's own NO_PROXY forgets those hosts.
	noProxy := mergeNoProxy(env("NO_PROXY", os.Getenv("no_proxy")), inCluster...)
	// NO_PROXY is injected even with no platform proxy: a workflow may bring its
	// own proxy, and without this list that proxy would also swallow in-cluster
	// traffic (cloning from Gitea, reporting to the facade).
	always := map[string]string{
		"NO_PROXY":              noProxy,
		"no_proxy":              noProxy,
		"GKR_PLATFORM_NO_PROXY": strings.Join(inCluster, ","),
	}
	if httpProxy == "" && httpsProxy == "" {
		return always
	}
	out := map[string]string{
		"NO_PROXY":           noProxy,
		"no_proxy":           noProxy,
		"NODE_USE_ENV_PROXY": "1",
	}
	out["no_proxy"] = out["NO_PROXY"]
	if httpProxy != "" {
		out["HTTP_PROXY"], out["http_proxy"] = httpProxy, httpProxy
	}
	if httpsProxy != "" {
		out["HTTPS_PROXY"], out["https_proxy"] = httpsProxy, httpsProxy
	}
	out["GKR_PLATFORM_NO_PROXY"] = always["GKR_PLATFORM_NO_PROXY"]
	return out
}

// noProxyHosts lists the in-cluster hosts a job pod must reach directly.
func (c *Config) noProxyHosts() []string {
	hosts := []string{"localhost", "127.0.0.1", "::1", "kubernetes.default.svc", ".svc", ".svc.cluster.local"}
	for _, raw := range []string{c.GiteaURL, c.ControllerURL, c.CacheServerURL} {
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			hosts = append(hosts, u.Hostname())
		}
	}
	return hosts
}

// mergeNoProxy appends the missing entries to a NO_PROXY list, preserving what
// the operator set and keeping each entry once.
func mergeNoProxy(base string, extra ...string) string {
	seen := map[string]bool{}
	var out []string
	for _, list := range append([]string{base}, extra...) {
		for _, e := range strings.Split(list, ",") {
			e = strings.TrimSpace(e)
			if e == "" || seen[e] {
				continue
			}
			seen[e] = true
			out = append(out, e)
		}
	}
	return strings.Join(out, ",")
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
