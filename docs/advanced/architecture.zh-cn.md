---
title: 架构与机制
slug: advanced/architecture
weight: 40
---

# 架构与机制

## 为什么是自研

| 现成方案 | 为什么不用 |
| --- | --- |
| Gitea 官方 runner 的 `kubernetes://` 模式 | 需要 k8s ≥ 1.30（靠 WebSocket `v5.channel.k8s.io` exec 进 job pod）；低版本上 Pod 建得起来但 exec 阶段被杀 |
| GARM + provider-k8s | provider-k8s / garm-operator 只认 GitHub/ARC 约定，没有 Gitea 实现 |
| Gitea Enterprise ARC | 商业版，靠 Enterprise 侧的 JIT/scale-set 协议 |

根因：**Gitea OSS 没有 JIT configuration API**——服务端拿不到"把指定 job 绑给指定 runner"的能力，
而 Gitea 自己的调度是"按 label 拉取"（`models/actions/task.go:CreateTaskForRunner`：按 label 子集筛
`status=Waiting` 的 job，谁先拉到归谁）。

## 核心机制：先领取 + facade 交接

既然服务端不能"指派"，那就在**客户端**把它做实：

1. **worker 先原子领取**：worker 本身是一条注册在册的 Gitea runner（声明池的 label）。有空位时调
   `FetchTask`，**原子领走一个具体 task**——Gitea 侧该 task 的归属当场确定，别的 worker 抢不走。
2. **交接给一次性环境**：controller 为该 task 建一个一次性 executor Pod；Pod 内的 **stock
   `gitea-runner`** 通过 controller 的 **facade** 拿到"这个已被领取的 task"并执行。
3. **回报**：Pod 内 runner 的 `UpdateTask`/`UpdateLog` 打到 facade，facade 用**领取者身份**转发给 Gitea。

### 为什么是 facade，而不是让 Pod 直连 Gitea

task 已经被 **worker 身份**领取了。Pod 内的 runner 若直连 Gitea：

- 它自己 `FetchTask` 拿不到这个 task（已被领取的不在 Waiting 里）；
- 用 worker 凭据直连，则 Gitea 只看到"一个 runner 跑了所有 job"，而且**把长期凭据下发到了 Pod**。

facade 让 stock runner **一行都不改**（`--instance` 指向 facade），长期凭据留在 controller；每个 Pod
只拿到一张**绑定本次 assignment 的 capability**（一次性、随 Pod 一起消失）。facade 还要反向代理非
RunnerService/Ping 的路径到真实 Gitea——因为 runner 会把 `--instance` 当成 job 的 server URL，
否则 `actions/checkout` 会去 facade 拉代码而 404。

## 组件

```
Gitea ◀─(worker 凭注册身份领取/回报)─ controller ──建 Pod──▶ executor Pod
                                                 │                 │
                                                 └── facade ◀──────┘（stock gitea-runner）
```

- **worker**：每个池一个，= 一条 Gitea runner 注册 + 一个领活循环 + 本地槽位（`slots`）。
- **facade**：每个 worker 一个，只把预领取的 task 交给持对应 capability 的 executor。
- **executor Pod**：一次性，跑完即删；命名 `gitea-runner-<repo>-<job>-<taskid>`，按 label 定位。
- **构建型 job**：Pod 里多一个官方 kaniko 容器（见[在 workflow 里构建镜像](../usage/image-building.zh-cn.md)）。

## 状态与恢复

两类状态，来源不同：

- **worker 的注册身份**（uuid + token）：存 k8s Secret，重启复用（否则每次重启都在 Gitea 里堆一条新记录）。
  注册顺序是**先证明 Secret 可写、再注册**——注册会产生一条只有 API 能删的记录，先注册后落盘失败会留僵尸。
- **in-flight assignment**：以 k8s 为状态源（Pod 的 label/annotation），不引数据库。

controller 启动时 **Recover**：把上一次进程留下的 executor Pod 全部清理并把对应 task 报失败供 Gitea
rerun（claim 模型下 facade 的内存态已丢，无法接管——这是 at-least-once 语义）。

Pod 起不来（`StartError` / `ImagePullBackOff` / 配置错）也会被判定为失败：这类状态会让 Pod **永远停在
Pending**，不处理就会占着槽位、并卡住 controller 的 drain 与滚动更新。

## 单例与 drain

- **副本必须为 1**：facade 的 `capability → assignment` 表是进程内存态，而 executor Pod 经 Service
  访问 facade，多副本会被负载均衡打到"不认识这个 task"的实例上。用 Lease 选主（`strategy: Recreate`），
  并发靠池的 `slots` 而不是副本数。
- **优雅关闭（drain）**：SIGTERM / 失去 leadership 时先停止领取新 task，在跑的 job 继续跑
  （job 的 context 不随 leader context 取消），最多等 `SHUTDOWN_TIMEOUT`（默认 30m，
  需 ≤ `terminationGracePeriodSeconds`）；超时才取消，并把被砍的 task 报失败供 rerun。

`controller` 是本仓库唯一的可执行文件（`gitea-runner-controller controller`）。
