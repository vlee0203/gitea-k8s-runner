# Gitea Actions Runner (k8s)

## Overview

A self-hosted [Gitea Actions](https://docs.gitea.com/usage/actions/overview) runner controller that executes
each job in a **one-shot Kubernetes Pod** — the same execution model as GitHub's Actions Runner Controller.
Workflows written to the GitHub Actions standard (`runs-on: ubuntu-latest`, `uses: actions/checkout@v4`,
`docker/build-push-action@v6`) run **unchanged**; image builds inside workflows are handled by a kaniko
sidecar container, so no docker daemon and no privileged container is required.

This Sealos template deploys **Gitea Actions Runner (k8s)** as the `gitea-k8s-runner` application. It provisions
a single-replica controller Deployment plus the ServiceAccount/Role it needs to create and delete executor Pods,
a Service that executor Pods use as their Gitea endpoint, a Secret holding the runner registration token, and a
ConfigMap holding the runner pool configuration.

## Deploy on Sealos

Open this template in the Sealos App Store, review the configuration values, and click **Deploy**. Sealos renders
the template variables, creates the required Kubernetes resources, and manages the endpoint for the application.

Two inputs are required:

- `gitea_url` — the Gitea instance the runners register to (must be reachable from the cluster).
- `registration_token` — a Gitea runner registration token. Use a **site-level** token
  (Administration → Actions → Runners) to serve every repository on the instance, or a user/repo-scoped token to
  serve less.

If that Gitea uses a **self-signed or corporate CA certificate**, do these two steps before pressing Deploy:

```bash
kubectl -n <the namespace Sealos shows for this app> \
  create secret generic gitea-ca --from-file=ca.crt=corp-ca.crt
```

and set `ssl_cert_file` to `/etc/gkr-ca/ca.crt`. (Only the controller needs to trust the CA: executor Pods talk
to the in-cluster Service over plain HTTP. See Configuration.)

## Access

This application has **no web UI** — it is a worker, not a web app. After deployment it appears in the Gitea
instance as one online runner per pool label set. Its desktop entry opens your Gitea instance, which is where
the registered runners show up:

```
Administration → Actions → Runners        (site-level)
```

To verify end to end, push this workflow to any Gitea repository:

```yaml
name: smoke
on: push
jobs:
  t:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: echo ok
```

A green job means the whole path works. `kubectl get pods` in the application namespace shows the one-shot
executor Pod appearing and disappearing again.

## Configuration

The following user-facing inputs are available during deployment:

| Name | Description | Required | Default |
|------|-------------|----------|---------|
| `gitea_url` | Gitea instance URL | `true` | — |
| `registration_token` | Runner registration token (site/user/repo scoped) | `true` | `<redacted>` |
| `controller_image` | Controller image (point at your own registry on an air-gapped cluster) | `true` | `ghcr.io/vlee0203/gitea-k8s-runner/controller:latest` |
| `runner_image` | Execution environment image (point at your own registry on an air-gapped cluster) | `true` | `ghcr.io/vlee0203/gitea-k8s-runner/runner:ubuntu-latest` |
| `kaniko_image` | kaniko image used when a workflow builds images | `true` | `ghcr.io/osscontainertools/kaniko:v1.28.6-debug` |
| `ssl_cert_file` | Set to `/etc/gkr-ca/ca.crt` when `gitea_url` uses a self-signed/private CA; leave empty otherwise | `false` | `` (empty) |
| `gitea_ca_secret` | Name of the CA Secret described above. Do **not** clear it | `true` | `gitea-ca` |
| `pool_cpu` / `pool_memory` | Per-job CPU / memory limit | `false` | `2` / `4Gi` |
| `pool_ephemeral_storage` | Per-job ephemeral storage. Keep it well above any namespace `LimitRange` default, otherwise checkout or image builds evict the Pod | `false` | `10Gi` |
| `pool_slots` | How many jobs run concurrently | `false` | `2` |
| `pool_timeout` | Per-job timeout (`1h`, `30m`, …) | `false` | `1h` |
| `http_proxy` / `https_proxy` / `no_proxy` | Egress proxy for executor Pods; in-cluster addresses are appended to `no_proxy` automatically | `false` | `` (empty) |

Keep sensitive values in Sealos-managed inputs or generated defaults. Do not commit private credentials to the
template repository.

Two more notes worth reading before you rely on this in production:

- **Runners must be able to pull `runner_image`.** On an air-gapped cluster mirror it into your own registry and
  point the input there; the same applies to `kaniko_image`. Workflow `docker build` steps pull their `FROM`
  base images at run time, so those must be reachable too.
- **If workflows push images to a Gitea container registry that uses a self-signed certificate**, the kaniko
  container (not just the controller) must trust that CA — use a derived kaniko image with the CA baked in and
  set `kaniko_image` accordingly.

## Official Links

- Source repository: <https://github.com/vlee0203/gitea-k8s-runner>
- Gitea Actions documentation: <https://docs.gitea.com/usage/actions/overview>
- GitHub Actions runner controller (the model this follows): <https://github.com/actions/actions-runner-controller>
