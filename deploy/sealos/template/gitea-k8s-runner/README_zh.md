# Gitea Actions Runner (k8s)

## 概述

自建的 [Gitea Actions](https://docs.gitea.com/usage/actions/overview) runner controller：每个 job 跑在一个
**一次性 Kubernetes Pod** 里（与 GitHub 的 ARC 同一套执行模型）。写标准 GitHub Actions 的 workflow
（`runs-on: ubuntu-latest`、`uses: actions/checkout@v4`、`docker/build-push-action@v6`）**原样可用**；
workflow 里构建镜像由同 Pod 的 kaniko 旁路容器完成，**不需要 docker daemon、不需要特权容器**。

本 Sealos 模板把 **Gitea Actions Runner (k8s)** 部署为 `gitea-k8s-runner` 应用，包含：单副本 controller
Deployment，以及它建/删一次性 Pod 所需的 ServiceAccount/Role；执行 Pod 当作 Gitea 地址用的 Service；
存注册令牌的 Secret；存 runner 池配置的 ConfigMap。

## 在 Sealos 上部署

在 Sealos 应用商店打开本模板，核对配置项后点 **Deploy**。Sealos 会渲染模板变量、创建所需资源并接管应用入口。

两个必填项：

- `gitea_url` —— runner 要注册到的 Gitea 地址（集群要能访问）。
- `registration_token` —— Gitea 的 runner 注册令牌。想要"全站所有仓库都能用"就用**站点级**令牌
  （管理后台 → Actions → Runners）；只服务某个用户/仓库就用对应作用域的令牌。

如果那个 Gitea 用的是**自签证书或企业 CA**，点 Deploy 前先做两步：

```bash
kubectl -n <Sealos 给这个应用分配的命名空间> \
  create secret generic gitea-ca --from-file=ca.crt=corp-ca.crt
```

并把 `ssl_cert_file` 填成 `/etc/gkr-ca/ca.crt`。（只有 **controller** 需要信任这个 CA：执行 Pod 全程只跟
集群内的 Service 说明文 HTTP。原因见「配置」。）

## 访问

本应用**没有 Web 界面** —— 它是 worker，不是 Web 应用。部署成功后，在 Gitea 里能看到对应的在线 runner
（每个池的 label 集合一个）。桌面入口点开跳转到你的 Gitea —— runner 就挂在那儿。

```
管理后台 → Actions → Runners        （站点级）
```

端到端验收：往任意 Gitea 仓库推这个 workflow：

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

job 变绿就说明整条链路通了；`kubectl get pods` 能在应用命名空间里看到那个一次性执行 Pod 出现又消失。

## 配置

部署时可填的用户侧参数：

| 名称 | 说明 | 必填 | 默认值 |
|------|------|------|--------|
| `gitea_url` | Gitea 实例地址 | `true` | — |
| `registration_token` | runner 注册令牌（站点级/用户级/仓库级） | `true` | 已打码 |
| `controller_image` | controller 镜像（内网集群指向自己的仓库） | `true` | `ghcr.io/vlee0203/gitea-k8s-runner/controller:latest` |
| `runner_image` | 执行环境镜像（内网集群指向自己的仓库） | `true` | `ghcr.io/vlee0203/gitea-k8s-runner/runner:ubuntu-latest` |
| `kaniko_image` | workflow 里构建镜像时用的 kaniko 镜像 | `true` | `ghcr.io/osscontainertools/kaniko:v1.28.6-debug` |
| `ssl_cert_file` | `gitea_url` 用自签/企业 CA 时填 `/etc/gkr-ca/ca.crt`，否则留空 | `false` | 空 |
| `gitea_ca_secret` | 上面那个 CA Secret 的名字。**不要清空** | `true` | `gitea-ca` |
| `pool_cpu` / `pool_memory` | 单个 job 的 CPU / 内存上限 | `false` | `2` / `4Gi` |
| `pool_ephemeral_storage` | 单个 job 的临时磁盘。要明显高于命名空间 `LimitRange` 的默认值，否则 checkout 或构建镜像会把 Pod 驱逐 | `false` | `10Gi` |
| `pool_slots` | 并发 job 数 | `false` | `2` |
| `pool_timeout` | 单 job 超时（`1h`、`30m`…） | `false` | `1h` |
| `http_proxy` / `https_proxy` / `no_proxy` | 执行 Pod 出网代理；集群内地址会自动追加进 `no_proxy` | `false` | 空 |

敏感值请放在 Sealos 托管的输入或自动生成项里，不要把私密凭据提交进模板仓库。

上生产前还有两点值得读一下：

- **runner 必须能拉到 `runner_image`。** 内网集群把它镜像进自己的仓库再把该输入指过去；`kaniko_image`
  同理。workflow 里 `docker build` 的 `FROM` 基础镜像是运行时拉的，也得能拉到。
- **如果 workflow 要往"用自签证书的 Gitea 容器仓库"推镜像**，那不只是 controller，**kaniko 容器**也得信任
  这个 CA —— 用一个把 CA 烧进去的派生 kaniko 镜像，并把 `kaniko_image` 指过去。

## 官方链接

- 项目源码：<https://github.com/vlee0203/gitea-k8s-runner>
- Gitea Actions 文档：<https://docs.gitea.com/usage/actions/overview>
- 参照的模型（GitHub ARC）：<https://github.com/actions/actions-runner-controller>
