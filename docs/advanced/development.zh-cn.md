---
title: 开发
slug: advanced/development
weight: 43
---

# 开发

## 代码结构

```
cmd/gitea-k8s-runner/     入口（controller 子命令）
internal/
  config/     环境变量 + pools.yaml 解析与校验
  scope/      namespace / cluster 两种作用域的差异收口（executor Pod 建在哪）
  worker/     一个池 = 一条 runner 身份 + 领活循环 + 槽位；含"这个 job 要构建镜像吗"的判定
  giteaclient/  Gitea RunnerService 客户端（Register/Declare/FetchTask/UpdateTask/UpdateLog + 身份头）
  executor/    executor Pod 的建/查/删（含构建型 job 的 kaniko 容器与共享目录）
  facade/      runner 交接（RunnerService + Ping 的本地实现，其余反向代理到真实 Gitea）
  lifecycle/   重启时的残留 Pod 清理与 task 失败上报
  state/       worker 身份持久化在 k8s Secret
container/
  controller.Dockerfile   controller 镜像（编译 + distroless）
  runner.Dockerfile       执行环境镜像（工具链 + stock gitea-runner + `docker` 替身）
  docker.sh               `docker` 替身（把标准 docker 命令转给同 Pod 的 kaniko 容器）
deploy/                   namespace / cluster 两套清单 + 可选的共享缓存
docs/                     本文档
```

## 本地构建与测试

工具链：**Go ≥ 1.26**（`gitea.dev/actionslib` 的要求）。本仓库在父级 `go.work` 下时，命令要带 `GOWORK=off`。

```bash
GOWORK=off go build ./...
GOWORK=off go test ./...        # 含 facade 交接语义、Pod 命名、启动失败判定、构建路由判定等单测
GOWORK=off go vet ./...
```

## 镜像与 CI

`.github/workflows/build.yaml`（push 到 main、且只在这些路径变化时触发：`container/` `cmd/` `internal/` `go.mod` `go.sum` 与 workflow 自身；也可手动触发）构建并推送两个镜像，**每个都是
`linux/amd64` + `linux/arm64` 双架构**：

| 镜像 | 来源 | 标签 |
| --- | --- | --- |
| `ghcr.io/vlee0203/gitea-k8s-runner/controller` | `container/controller.Dockerfile` | `sha-<短 commit sha>`、`latest`（main 上）；打 `vX.Y.Z` tag 时另有 `vX.Y.Z` |
| `ghcr.io/vlee0203/gitea-k8s-runner/runner` | `container/runner.Dockerfile` | 同上，另有历史沿用的 `ubuntu-latest` |

**部署清单钉的是不可变的 `sha-<短 commit sha>` 标签**，不要钉 `latest` / `ubuntu-latest` —— 那些会被下一次构建覆盖；
标签里 `sha-<短 sha>` 也是不可变的，想用可读一点的写法就换成它。

多架构不额外花钱：构建阶段用 `--platform=$BUILDPLATFORM` 在 builder 上交叉编译（Go 目标架构来自
`TARGETOS/TARGETARCH`），不需要 QEMU 跑编译；两个基础镜像（`golang` / `distroless`、以及 runner 的
`docker.gitea.com/runner` 与 `runner-images`）本身都发布双架构。

该 workflow 本身用标准 GitHub 写法（`checkout` + `setup-qemu` + `setup-buildx` + `login` + `metadata` +
`build-push`），**也能原样跑在 Gitea 上**（只需把 registry 与登录凭据换成 Gitea 的），见
[在 workflow 里构建镜像](../usage/image-building.zh-cn.md)。**但多平台那一步在 Gitea 侧跑不了**：
集群里没有 binfmt/QEMU 注册且安全基线不允许特权，平台的 `docker` 替身会明确报错而不是静默降级
（自举构建请在 GitHub 上跑，或临时把 `platforms` 改成单架构）。

## 改动时注意

- executor Pod 的镜像**强制 `PullAlways`**（池镜像用可变 tag，缓存会掩盖改动）。
- 新增/修改池字段时同步更新 `deploy/*/all.yaml` 的示例与本文档。
- 涉及机制性取舍（为什么这样做、不做会怎样）写成**紧邻代码的那几行注释**，别只留在文档里。
