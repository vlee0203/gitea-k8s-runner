---
title: 部署总览
slug: installation/overview
weight: 20
---

# 部署

同一个二进制、同一个镜像，**差别只在注册令牌的作用域**——令牌的作用域决定这个 controller 服务谁。

## 前置条件

| 项 | 要求 |
| --- | --- |
| Kubernetes | 1.20+；命名空间 PSA `baseline` 即可（**不需要** privileged、不需要挂 docker socket、不需要 CRD） |
| Gitea | 带 Actions 的版本（本仓库实测 1.27.3 / 28.1.0） |
| 权限 | 用户级形态：目标 ns 的 Role 即可；集群级形态：ClusterRole |
| 出口网络 | Pod 需要能访问 Gitea 与镜像仓库；构建镜像时还需能拉构建所依赖的基础镜像（以及 workflow 里 `uses:` 的 action 源） |

## 三种作用域形态

| 形态 | 注册令牌 | 服务范围 | 清单 |
| --- | --- | --- | --- |
| **用户级**（自助、最小 RBAC） | user 级 | 该用户名下**所有**仓 | `deploy/namespace/all.yaml` |
| **站点级**（平台自带 runner） | site 级 | **全站所有仓**，等价 GitHub-hosted | `deploy/cluster/all.yaml` |
| 仓库级（最小权限） | repo 级 | 仅该仓 | 同上，换令牌 |

令牌来源（在 Gitea 里取）：

```
repo 级  POST /repos/{owner}/{repo}/actions/runners/registration-token
user 级  POST /user/actions/runners/registration-token
site 级  POST /admin/actions/runners/registration-token
```

**作用域隔离是这套设计的基础**：平台 runner 注册在自己的作用域里，用户自己注册的 runner 在各自的
user/repo 作用域，两边清单天然分开（见[安全模型](../advanced/security.zh-cn.md)）。

## 部署步骤

```bash
# 1. 令牌放进 Secret（controller 经 k8s API 读，不落盘、不随 Pod 分发）
kubectl -n <ns> create secret generic gitea-runner-registration --from-literal=token=<TOKEN>

# 2. 按需改清单：
#    GITEA_INSTANCE_URL  —— 真实 Gitea 地址
#    FACADE_URL          —— 集群内可达本 controller Service 的地址
#    pools.yaml          —— 池（labels / image / resources / slots / timeout）
kubectl apply -f deploy/namespace/all.yaml    # 或 deploy/cluster/all.yaml

# 3.（可选）共享缓存服务器：跨 job 命中 actions/cache，否则每个一次性 Pod 都是冷缓存
kubectl apply -f deploy/cache.yaml
```

`pools.yaml` 示例（labels 的含义与匹配规则见[用法](../usage/overview.zh-cn.md)）：

```yaml
pools:
  - labels: [self-hosted, linux, x64, ubuntu-latest, ubuntu-24.04]
    image: ghcr.io/vlee0203/gitea-k8s-runner/runner:ubuntu-latest
    resources: { cpu: "2", memory: 4Gi, ephemeralStorage: 10Gi }
    slots: 2
    timeout: 1h
```

> `resources` 里的 `ephemeralStorage` **必须给足**：命名空间若有 LimitRange 默认值（常见 100Mi），
> checkout 或构建镜像时会直接撑爆并**驱逐 Pod**（表现为 job 莫名其妙被取消）。

## 出网与代理

job Pod 要访问三类地址：**Gitea**（拉代码）、**controller 的 facade**、以及**外网**（`uses:` 的 action、
构建镜像的基础镜像、包仓库）。如果外网需要代理，分三层配：

| 层 | 谁配 | 怎么配 |
| --- | --- | --- |
| 节点拉 Pod 镜像 | 集群/节点管理员 | containerd 的代理配置（systemd 环境变量或 `config.toml`）—— 这一层不归本平台管 |
| **executor Pod 里的网络** | 部署 controller 的人（**平台已支持**） | 在 controller 上设 `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`，平台会把它们注入**每个** executor Pod（runner 与 kaniko 两个容器都注入）；`NODE_USE_ENV_PROXY=1` 也会自动加上（Node ≥ 20 的 `fetch` 默认不认环境变量里的代理，而执行环境镜像是 Node 24） |
| action 源码从哪来 | Gitea 管理员 | Gitea 默认从 `github.com` 取 action；不想让 Pod 出网就把它指向自己：`[actions] DEFAULT_ACTIONS_URL = self`（action 从 Gitea 内网取，完全不吃代理） |

```yaml
# controller 的 env（示例见 deploy/*/all.yaml 的注释）
- { name: HTTP_PROXY,  value: "http://proxy.example.com:3128" }
- { name: HTTPS_PROXY, value: "http://proxy.example.com:3128" }
- { name: NO_PROXY,    value: "10.0.0.0/8,.svc.cluster.local,.example.com" }
```

平台会**自动**把 `localhost,127.0.0.1,::1,kubernetes.default.svc,.svc,.svc.cluster.local` 以及
`GITEA_INSTANCE_URL` / `FACADE_URL` / `CACHE_SERVER_URL` 的主机名追加进 `NO_PROXY`——把集群内流量
送去代理是这类环境里最常见的翻车方式。（kaniko 认 `HTTP_PROXY`/`HTTPS_PROXY`：实测把代理指到死端口，
它会报 `proxyconnect … connection refused`。）

### 用 `self` 就必须同时关掉开放注册与建组织

`DEFAULT_ACTIONS_URL = self` 把 `uses: <owner>/<repo>@<ref>` 的解析权交给自己的实例，而**上游 owner 名在
Gitea 侧没有任何保护**（源码 `reservedUsernames` 只挡 Gitea 自己的路由名；没有 `RESERVED_USERNAMES`
配置项；用户名字符集 `^[\da-zA-Z][-.\w]*$` 允许连字符）。默认配置下任何人都能自助注册、并建组织，
于是抢注 `aws-actions` / `hashicorp` / `github` 这类还没被占的名字，就能让写 `uses: aws-actions/…@v4`
的 job 拉到攻击者的代码。**Gitea 侧必须一起设**：

```
GITEA__service__DISABLE_REGISTRATION=true          # 断掉陌生人注册
GITEA__openid__ENABLE_OPENID_SIGNUP=false          # 内置 OpenID 注册是另一条口子
GITEA__admin__DISABLE_REGULAR_ORG_CREATION=true    # 非管理员不能建组织
```

（`kubectl set env sts/<gitea> …` 即 env-to-ini，会重启一次 Gitea。组织名与用户名**同一命名空间**，
所以已占的名字别人抢不走 —— 风险只在"没人占的名字"。）机制与取舍见
[平台公共 action](../advanced/actions-mirror.zh-cn.md)。

## 部署后校验

1. controller 日志应出现 `registered worker "<name>" for [<labels>]` 与
   `controller leading: … pools=N`。
2. Gitea 侧对应作用域的 Runners 页面能看到这些 runner 且状态 `online`
   （或 `GET /api/v1/{user,admin,repos/...}/actions/runners`）。
3. 跑一个最小 workflow 验收：

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

   job 成功后，`kubectl get pods` 里应短暂出现 `gitea-runner-<repo>-<job>-<taskid>` 并随后被删除。

## 两种清单的差异

| | `namespace/all.yaml` | `cluster/all.yaml` |
| --- | --- | --- |
| 权限 | Role / RoleBinding（本 ns） | ClusterRole / ClusterRoleBinding |
| 目标 ns | 清单所在 ns（`TARGET_NAMESPACE` 可用 fieldRef 取自己） | `TARGET_NAMESPACE` 指定的 ns（v1 不跨 ns 映射） |
| 其余资源 | 完全相同 | 完全相同 |

## 升级与回滚

改配置（env、pools、镜像 tag）后 `kubectl apply` 即可：Deployment 用 `strategy: Recreate` +
`terminationGracePeriodSeconds: 3660`，controller 在退出前会**先等在跑的 job 跑完**（drain，
上限 `SHUTDOWN_TIMEOUT`，默认 30m）。所以"发布要等一会儿"是设计行为，不是卡住；
要"发布优先"就把 `SHUTDOWN_TIMEOUT` 调小（代价是发布时砍掉在跑的 job，Gitea 侧失败可 rerun）。
