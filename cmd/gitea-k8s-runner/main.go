// Command gitea-k8s-runner runs Gitea Actions jobs as one-shot Kubernetes pods.
// A worker claims a task from Gitea directly (so the task cannot be taken by
// another job), then hands that exact task to a fresh executor pod through a
// local RunnerService facade.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/vlee0203/gitea-k8s-runner/internal/config"
	"github.com/vlee0203/gitea-k8s-runner/internal/executor"
	"github.com/vlee0203/gitea-k8s-runner/internal/facade"
	"github.com/vlee0203/gitea-k8s-runner/internal/giteaclient"
	"github.com/vlee0203/gitea-k8s-runner/internal/lifecycle"
	"github.com/vlee0203/gitea-k8s-runner/internal/scope"
	"github.com/vlee0203/gitea-k8s-runner/internal/state"
	"github.com/vlee0203/gitea-k8s-runner/internal/worker"
)

// leaseName is the mutual-exclusion lock. Only the holder claims new tasks: a
// claimed task can only be reported by the runner identity that claimed it, and
// each pool has exactly one identity. Serving is not exclusive — the facade
// resolves every call from the assignment's Secret, so any instance (including a
// fresh replacement) can serve any pod.
const leaseName = "gitea-k8s-runner"

// drainSweepInterval is how often a draining leader re-checks whether any of its
// in-flight jobs has gone quiet long enough to be considered wedged.
const drainSweepInterval = 15 * time.Second

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: gitea-k8s-runner controller")
	}
	switch args[0] {
	case "controller":
		return runController(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runController(args []string) error {
	fs := flag.NewFlagSet("controller", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	sc, err := scope.New(cfg.Scope, cfg.TargetNamespace)
	if err != nil {
		return err
	}
	cs, err := kubeClient()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	identity, err := os.Hostname()
	if err != nil {
		return err
	}
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: leaseName, Namespace: sc.ExecutorNamespace()},
		Client:     cs.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: identity},
	}
	// RunOrDie returns as soon as ctx is cancelled, but lead() may still be
	// draining in-flight jobs. Exiting here would kill that drain, so wait for
	// the leader callback to finish before returning.
	var leading atomic.Bool
	leadDone := make(chan struct{})
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock: lock,
		// 释放：服务是无状态的（facade 从 assignment 的 Secret 解析），所以新实例
		// 一拿到租约就能既服务老实例手上的 job、又开始领新活；老实例继续 drain 它自己
		// 的 job，两边并行不冲突。
		ReleaseOnCancel: true,
		LeaseDuration:   30 * time.Second,
		RenewDeadline:   20 * time.Second,
		RetryPeriod:     5 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leaderCtx context.Context) {
				leading.Store(true)
				defer close(leadDone)
				lead(cfg, sc, cs, leaderCtx)
			},
			OnStoppedLeading: func() { log.Printf("leadership lost") },
		},
	})
	if leading.Load() {
		<-leadDone
	}
	return nil
}

// lead runs the controller while this instance holds the lease. When leaderCtx
// is cancelled (signal or lost lease) it stops claiming new work and lets
// in-flight jobs drain before they are cancelled.
func lead(cfg *config.Config, sc scope.Scope, cs kubernetes.Interface, leaderCtx context.Context) {
	// jobCtx outlives leaderCtx: in-flight jobs keep running during the drain.
	jobCtx, cancelJobs := context.WithCancel(context.WithoutCancel(leaderCtx))
	defer cancelJobs()

	store := state.New(cs, sc.ExecutorNamespace(), cfg.StateSecretName)
	exec := executor.New(cs, sc.ExecutorNamespace())
	// 池身份表：facade 解析一条 assignment 时要用它把回报交给"领取它的那个 runner"。
	// 每个实例都持有全部池的身份（同一份 pools 配置），所以任何实例都能服务任何 job。
	poolClients := map[string]*giteaclient.Client{}
	fac := facade.New(facade.NewK8sResolver(cs, sc.ExecutorNamespace(), func(poolKey string) facade.Upstream {
		if c := poolClients[poolKey]; c != nil {
			return c
		}
		return nil
	}))
	httpClient := &http.Client{Timeout: 60 * time.Second}

	// Prove the state Secret is writable before registering anything: a failed
	// registration followed by a failed persist leaves a zombie runner in Gitea
	// on every restart.
	if err := store.EnsureWritable(leaderCtx); err != nil {
		fatal("state secret %q is not writable: %v", cfg.StateSecretName, err)
	}

	// tokenFor returns a pool's registration token. When a Secret name is
	// configured the token is read through the API, so it is never written to
	// the pod filesystem; otherwise the pool's mounted file is used.
	tokenFor := func(ctx context.Context, pool config.Pool) (string, error) {
		if cfg.RegistrationTokenSecret == "" {
			return readToken(cfg.PoolTokenFile(pool))
		}
		sec, err := cs.CoreV1().Secrets(sc.ExecutorNamespace()).Get(ctx, cfg.RegistrationTokenSecret, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("read registration token secret %s: %w", cfg.RegistrationTokenSecret, err)
		}
		return strings.TrimSpace(string(sec.Data["token"])), nil
	}

	clients := make([]*giteaclient.Client, 0, len(cfg.Pools))
	workers := make([]*worker.Worker, 0, len(cfg.Pools))
	var wg sync.WaitGroup
	for i := range cfg.Pools {
		pool := cfg.Pools[i]
		cli := giteaclient.New(cfg.GiteaURL, httpClient)

		key := cfg.PoolStateKey(pool)
		id, ok, err := store.Get(leaderCtx, key)
		if err != nil {
			fatal("read worker state: %v", err)
		}
		if !ok {
			token, err := tokenFor(leaderCtx, pool)
			if err != nil {
				fatal("pool %v: %v", pool.Labels, err)
			}
			r, err := cli.Register(leaderCtx, cfg.WorkerName+"-"+key, token, pool.Labels, false)
			if err != nil {
				fatal("register worker for %v: %v", pool.Labels, err)
			}
			id = state.Identity{UUID: r.Uuid, Token: r.Token}
			if err := store.Put(leaderCtx, key, id); err != nil {
				fatal("persist worker state: %v", err)
			}
			log.Printf("registered worker %q for %v", r.Name, pool.Labels)
		}
		cli.SetIdentity(id.UUID, id.Token)
		poolClients[key] = cli
		if err := cli.Declare(leaderCtx, pool.Labels); err != nil {
			fatal("declare %v: %v", pool.Labels, err)
		}
		clients = append(clients, cli)

		w := worker.New(cfg, pool, cli, fac, exec)
		workers = append(workers, w)
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.Run(leaderCtx, jobCtx)
		}()
	}

	// Executors left over from a predecessor are adopted: their assignments live in
	// their Secrets, so this instance can serve them without failing the job. Only
	// pods whose assignment (or pool) is gone get deleted.
	adopters := map[string]lifecycle.Adopter{}
	for i := range cfg.Pools {
		adopters[cfg.PoolStateKey(cfg.Pools[i])] = workers[i]
	}
	lifecycle.New(exec, adopters).Recover(jobCtx)

	// An executor pod points its runner at this address (the facade), so the
	// runner also treats it as the job's server URL — e.g. actions/checkout
	// clones from here. Proxy everything that is not the runner protocol itself
	// to the real Gitea so those requests still work.
	giteaURL, err := url.Parse(cfg.GiteaURL)
	if err != nil {
		fatal("parse GITEA_INSTANCE_URL: %v", err)
	}
	mux := http.NewServeMux()
	_, runnerH := fac.Handler()
	_, pingH := fac.PingHandler()
	mux.Handle("/api/actions/runner.v1.RunnerService/", http.StripPrefix("/api/actions", runnerH))
	mux.Handle("/api/actions/ping.v1.PingService/", http.StripPrefix("/api/actions", pingH))
	mux.Handle("/", newGiteaProxy(giteaURL))

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("facade server: %v", err)
			cancelJobs()
		}
	}()
	log.Printf("controller leading: leader=%s scope=%s ns=%s facade=%s pools=%d",
		os.Getenv("HOSTNAME"), sc.Kind(), sc.ExecutorNamespace(), cfg.ListenAddr, len(cfg.Pools))

	<-leaderCtx.Done()
	log.Printf("draining in-flight jobs (timeout %s, 单 job 静默上限 %s)", cfg.ShutdownTimeout, cfg.DrainSilence)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	// 卡死的 job 不能一直占着 drain（它会挡住这次发布）：只掐那些静默超上限的，
	// 还在上报的 job 照旧等它跑完。被掐的 job 会走 worker 里的 ctx.Done 分支
	// —— 报 task 失败 + 删 pod。
	sweep := time.NewTicker(drainSweepInterval)
	defer sweep.Stop()
	deadline := time.After(cfg.ShutdownTimeout)
drain:
	for {
		select {
		case <-done:
			log.Printf("all in-flight jobs finished")
			break drain
		case <-sweep.C:
			// 等待条件必须包含"接管的 job"：它们不产生 worker goroutine，只看 wg 会在
			// 只有接管 job 时立刻判定 drain 结束（实测踩过：这样一个 job 一直留到实例退出）。
			remaining := int64(0)
			for _, w := range workers {
				w.CancelSilentJobs(leaderCtx, cfg.DrainSilence)
				remaining += w.Running()
			}
			if remaining == 0 {
				log.Printf("all in-flight jobs finished")
				break drain
			}
		case <-deadline:
			log.Printf("drain timed out: cancelling in-flight jobs")
			cancelJobs()
			<-done
			break drain
		}
	}
	cancelJobs()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

// newGiteaProxy forwards everything that is not the runner protocol to the real
// Gitea. It must rewrite the outbound Host to the target's: the stock
// NewSingleHostReverseProxy **preserves the inbound Host** (Go's own doc says so),
// so the upstream would only ever see this controller's service name. A Gitea
// behind a Host-routing gateway then answers "no route" — 404 with Server:
// istio-envoy, which looks nothing like a Gitea error (measured: same path
// direct = 200, through the facade = 404).
//
// Rewrite + SetURL does the rewrite: SetURL clears Out.Host, so the Transport
// falls back to URL.Host, i.e. GITEA_INSTANCE_URL's host. Director and Rewrite are
// mutually exclusive — NewSingleHostReverseProxy sets Director, so it must be
// cleared first, otherwise every proxied request fails with
// "ReverseProxy must have exactly one of Director or Rewrite set" (502).
func newGiteaProxy(giteaURL *url.URL) *httputil.ReverseProxy {
	p := httputil.NewSingleHostReverseProxy(giteaURL)
	p.Director = nil
	p.Rewrite = func(r *httputil.ProxyRequest) { r.SetURL(giteaURL) }
	return p
}

// fatal reports a configuration or startup error and exits. A misconfigured
// leader must crash so it does not hold the lease while doing nothing.
func fatal(format string, args ...any) {
	log.Printf("fatal: "+format, args...)
	os.Exit(1)
}

// readToken reads a registration token file, trimming the trailing newline.
func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read registration token %s: %w", path, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// kubeClient builds a client from the in-cluster config, falling back to the
// default kubeconfig (so a user-scoped deployment can run outside a cluster).
func kubeClient() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
		if err != nil {
			return nil, err
		}
	}
	return kubernetes.NewForConfig(cfg)
}
