---
title: 在 workflow 里构建镜像
slug: usage/image-building
weight: 31
---

# 在 workflow 里构建并推送镜像

**按 GitHub 写就行**，本平台不需要对 workflow 做任何改动：

```yaml
- uses: actions/checkout@v4
- run: docker login $REGISTRY -u '${{ secrets.REG_USER }}' -p '${{ secrets.REG_PASS }}'
- run: docker build -t $IMAGE:$TAG -t $IMAGE:latest .
- run: docker push $IMAGE:$TAG
```

官方 docker action 也照原样可用（已实测跑通）：

```yaml
- uses: docker/setup-qemu-action@v3      # 跳过并告警（本平台不做多平台模拟，见下）
- uses: docker/setup-buildx-action@v3    # 虚拟 builder（真正构建仍由 kaniko 完成）
- uses: docker/login-action@v3
- uses: docker/metadata-action@v5
- uses: docker/build-push-action@v6
  with: { context: ., file: ./Dockerfile, platforms: linux/amd64, push: true, tags: ... }
```

## 平台是怎么接住的

本平台没有任何 docker daemon（部署集群的命名空间是 PSA `baseline`，不许特权容器；docker / podman /
buildah 都要挂载文件系统，没有 `CAP_SYS_ADMIN` 干不了）。唯一能在这种限制下构建镜像的是 **kaniko**，
而 kaniko 官方要求**作为它自己的镜像运行**（它会清空自己容器的根文件系统来解包基础镜像）。

所以流程是：

1. controller 读 workflow，判断这个 job 要构建镜像（出现 `docker build/buildx/push/login`、
   `docker/*` 官方 action、或直接调 kaniko）；
2. 该 job 的 Pod 里**多一个容器**，跑**官方 kaniko 镜像**（待命；无特权、无 daemon、无 socket；
   仍然是"每 job 一个一次性 Pod"）；
3. runner 容器里的 **`docker` 替身**（`container/docker.sh`）把标准 docker 命令转成对那个容器的请求
   （共享目录 `/shared/req/<id>/{args,go,log,rc,done}`），并把 kaniko 的日志流回 job 日志、以它的退出码结束；
4. runner 容器的 `HOME` 指向共享目录，`actions/checkout` 的工作区即构建上下文，两边都看得见。

替身为了配合官方 action 还提供：裸 `docker buildx`（它们用它判断 buildx 可用性）、`buildx inspect`
（返回其逐行解析的表格）、`context inspect`（JSON）、`version/info --format`、`image inspect`/`images`、
`pull`（跳过+告警）、`run`（**只**跳过 binfmt/QEMU 注册调用，其它明确报错）、
`--metadata-file`/`--iidfile`（从 kaniko 日志的 `Pushed …@sha256:` 取 digest，写成 action 读的元数据）。

## 代理

平台侧（在 controller 上设 `HTTP_PROXY/HTTPS_PROXY/NO_PROXY`，平台注入每个 Pod）见[部署](../installation/overview.zh-cn.md)。

**workflow 也可以自带代理**（`env:`，或仓库 secret/variable 里给）——两条路径都通：

- 它作用在**步骤**上（含 JS action；Node ≥ 20 还需要 `NODE_USE_ENV_PROXY=1` 才会用它）。
- 构建部分：替身会把本次步骤看到的代理设置**一并转交给 kaniko 容器**，所以拉基础镜像、推镜像也走它。
  转交时 `NO_PROXY` 取「workflow 那份 + 平台那份（`GKR_PLATFORM_NO_PROXY`）」的**并集**——即使 workflow
  里漏写集群内地址，也不会把 Gitea / facade 的流量送进代理。

实测（workflow 里把 `HTTPS_PROXY` 指到一个死端口）：

```
NO_PROXY=localhost,127.0.0.1,::1,kubernetes.default.svc,.svc,.svc.cluster.local,gitea-xejbddyy,...   ← 平台注入
[docker] 转交代理设置给 kaniko 容器：HTTP_PROXY=http://127.0.0.1:9 HTTPS_PROXY=http://127.0.0.1:9 NO_PROXY=...
error building image: Get "https://docker.gitea.com/v2/": proxyconnect tcp: dial tcp 127.0.0.1:9: connect: connection refused
```

同一次运行里 `actions/checkout@v4` 照常成功——集群内地址在 `NO_PROXY` 里，不受用户代理影响。

## 已知边界

| 边界 | 说明 |
| --- | --- |
| **多平台构建**（`platforms: linux/amd64,linux/arm64`） | 做不到：需要 binfmt/QEMU 模拟，而注册模拟器要 `docker run --privileged`，安全基线不允许。平台会**明确报错并说明原因**（不静默忽略） |
| `docker build` + 单独 `docker push` | 能跑，但 kaniko 没有本地镜像库，`push` 会**按同样参数重新构建一次**再推（多个 tag 会一次推完，不会推多个 tag 就构建多遍） |
| `docker run` / `docker compose` / `services:` / `uses: docker://` | 需要真 daemon，明确报错 |
| 构建缓存 | `cache-from/to type=gha` 无对应；kaniko 支持 `--cache/--cache-repo`（registry 缓存），当前替身未启用 |

## 两个会伪装成"别的问题"的坑

- **`ephemeralStorage` 不足 → 构建中途被驱逐**：kaniko 解包基础镜像+打快照需要 GB 级临时空间；
  池里没写、命名空间有 LimitRange 默认值（常见 100Mi）时，表现为 job 跑到一半被 **cancelled**，
  事件里写着 `Container kaniko exceeded its local ephemeral storage limit`。池里显式给大即可。
- **池镜像 tag 可变 + 默认拉取策略 → 改了镜像没生效**：`runner:ubuntu-latest` 这类可变 tag 下，
  `IfNotPresent` 会让 Pod 一直用节点上缓存的旧镜像（症状：新加的能力"看不见"）。平台的 executor Pod
  已强制 `PullAlways`；如果你自己加了别的池镜像，注意这一点。
