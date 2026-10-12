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
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/vlee0203/gitea-k8s-runner/internal/config"
	"github.com/vlee0203/gitea-k8s-runner/internal/executor"
	"github.com/vlee0203/gitea-k8s-runner/internal/facade"
	"github.com/vlee0203/gitea-k8s-runner/internal/giteaclient"
	"github.com/vlee0203/gitea-k8s-runner/internal/lifecycle"
	"github.com/vlee0203/gitea-k8s-runner/internal/scope"
	"github.com/vlee0203/gitea-k8s-runner/internal/state"
	"github.com/vlee0203/gitea-k8s-runner/internal/worker"
)

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

	regToken, err := os.ReadFile(cfg.RegistrationTokenFile)
	if err != nil {
		return fmt.Errorf("read registration token: %w", err)
	}
	token := strings.TrimSpace(string(regToken))

	store := state.New(cs, sc.ExecutorNamespace(), cfg.StateSecretName)
	fac := facade.New()
	exec := executor.New(cs, sc.ExecutorNamespace())
	httpClient := &http.Client{Timeout: 60 * time.Second}

	clients := make([]*giteaclient.Client, 0, len(cfg.Pools))
	var wg sync.WaitGroup
	for i := range cfg.Pools {
		pool := cfg.Pools[i]
		cli := giteaclient.New(cfg.GiteaURL, httpClient)

		// Reuse this worker's persisted runner identity, or register a new one.
		key := poolKey(pool)
		id, ok, err := store.Get(ctx, key)
		if err != nil {
			return err
		}
		if !ok {
			r, err := cli.Register(ctx, cfg.WorkerName+"-"+key, token, pool.Labels, false)
			if err != nil {
				return fmt.Errorf("register worker for %v: %w", pool.Labels, err)
			}
			id = state.Identity{UUID: r.Uuid, Token: r.Token}
			if err := store.Put(ctx, key, id); err != nil {
				return err
			}
			log.Printf("registered worker %q for %v", r.Name, pool.Labels)
		}
		cli.SetIdentity(id.UUID, id.Token)
		if err := cli.Declare(ctx, pool.Labels); err != nil {
			return fmt.Errorf("declare %v: %w", pool.Labels, err)
		}
		clients = append(clients, cli)

		w := worker.New(cfg, pool, cli, fac, exec)
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.Run(ctx)
		}()
	}

	// Clean up executors left over from a previous run before starting fresh.
	lifecycle.New(exec, clients).Recover(ctx)

	// An executor pod points its runner at this address (the facade), so the
	// runner also treats it as the job's server URL — e.g. actions/checkout
	// clones from here. Proxy everything that is not the runner protocol itself
	// to the real Gitea so those requests still work.
	giteaURL, err := url.Parse(cfg.GiteaURL)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	_, runnerH := fac.Handler()
	_, pingH := fac.PingHandler()
	mux.Handle("/api/actions/runner.v1.RunnerService/", http.StripPrefix("/api/actions", runnerH))
	mux.Handle("/api/actions/ping.v1.PingService/", http.StripPrefix("/api/actions", pingH))
	mux.Handle("/", httputil.NewSingleHostReverseProxy(giteaURL))
	srv := &http.Server{Addr: cfg.ListenAddr, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("facade server: %v", err)
			stop()
		}
	}()
	log.Printf("controller ready: scope=%s ns=%s facade=%s pools=%d", sc.Kind(), sc.ExecutorNamespace(), cfg.ListenAddr, len(cfg.Pools))

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	wg.Wait()
	return nil
}

// poolKey derives a stable key from a pool's labels. Each pool gets its own
// runner identity, so labels are the natural identity key.
func poolKey(pool config.Pool) string {
	return strings.ReplaceAll(strings.Join(pool.Labels, "-"), "/", "_")
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
