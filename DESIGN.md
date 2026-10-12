# gitea-k8s-runner 设计文档

> 状态：**草案 v0.1**（待评审）。项目名暂定，仓库未建。

## 1. 背景与目标

在 sealos 多租户 k8s 上，为 **Gitea Actions** 提供执行环境，要求：

1. **每个 job 一个一次性 Pod**（跑完连同本地状态一起销毁），类 GitHub ARC。
2. **job ↔ runner 精确绑定**：不做"按 label 拉取"的近似调度——某个 job 由哪个执行环境跑，必须是确定的、可归属的。
3. workflow 以 **GitHub Actions 标准**为准：`runs-on: ubuntu-latest`、`uses: actions/checkout@v4` 必须直接可用。

### 为什么必须自己写

| 现成方案 | 为什么不可用（均已核实） |
| --- | --- |
| 官方 runner `kubernetes://` | 需要 k8s ≥1.30：它用 WebSocket `v5.channel.k8s.io` exec 进 job pod（`act/container/kubernetes_client.go:32,314`）。我们用 1.28.11，实测 pod 建得起来但在 exec 阶段被杀。 |
| GARM + provider-k8s | provider-k8s / garm-operator **只认 GitHub/ARC 约定**（注入 `RUNNER_TOKEN`/`GITHUB_URL`/`JIT_CONFIG_ENABLED`），全仓无 Gitea 代码；GARM 对 Gitea 只做了 VM/LXD 的 userdata 落地。 |
| Gitea Enterprise ARC | 是**商业版**（>23.8.0 且需授权），且它靠的是 Enterprise 侧提供的 JIT/scale-set 协议。 |
| gitea-dynamic-runner (panxiao81) | 方向正确，但强依赖 **SPIRE/SPIFFE + OpenBao + privileged Pod**，且**仓库无 LICENSE**；其复杂度大量来自身份体系，与我们要解决的问题不成比例。 |

根因一句话：**Gitea OSS 没有 JIT configuration API**（1.27.3 swagger 里 runner 相关只有 list/get/delete + `registration-token`，`jit: []`）。没有 JIT，就无法让服务端把"指定的 job"绑给"指定的 runner"。GitHub/Gitea Enterprise 的 ARC 正是靠 JIT 解决"抢占/归属"的。

## 2. 约束

- k8s **v1.28.11**（usw；hzh 为 1.28.9），节点是 containerd，**无 docker daemon**。
- 命名空间 PSA `enforce=baseline` → **不能 privileged、不能 SYS_ADMIN**。
- Gitea **OSS 1.27.3**，runner 协议见 `actionslib` / `actions-proto-go`。
- 两种部署形态（见 §8）：**用户级**（只用 `ns-xxx` 的 kubeconfig/RBAC）与**集群级**（ClusterRole）。

## 3. 非目标（v1 明确不做）

- 不做 DinD / Docker-in-Docker、不做 Cloud Hypervisor microVM。
- 不引入 SPIFFE/SPIRE、OpenBao。
- 不 fork 官方 runner。
- v1 不做 Gitea 仓库 → sealos `ns-<uid>` 的映射（集群级也先建到 controller 自己的 ns）。
- v1 不解决"在 workflow 里构建镜像"（见 §11，单独设计）。

## 4. 核心机制：JIT 在 OSS 上的等价物 = 先领取 + facade 交接

Gitea 的调度是**按 label 拉取**（`models/actions/task.go:CreateTaskForRunner`：筛 `status=Waiting` + runner 的 repo/owner scope，按 `updated,id` 从旧到新取第一个 `CanMatchLabels(runs_on)` 命中的 job）。所以"注册一个临时 runner 去竞争"**做不到精确归属**——它可能领走别的等待中 job。

等价 JIT 的做法：

1. **worker 主动领取**：worker 本身是一条已注册的 Gitea runner（声明池标签）。它在**有空位时**调用 `FetchTask`，**原子领走一个具体 task**——此刻 Gitea 已把该 task 的 `runner_id` 设为这个 worker，归属**已确定**。
2. **交接给一次性执行环境**：controller 为这个已领取的 task 建一个 Pod；Pod 里的 **stock `gitea-runner`** 通过 **facade**（controller 实现的一个 Gitea RunnerService 端点）拿到"这个已被领取的 task"并执行。
3. **回报**：Pod 内 runner 的 `UpdateTask`/`UpdateLog` 发到 facade，facade 用**领取者身份**转发给 Gitea。

要点：worker 一次只领一个（或按 capacity 领固定数量），领到的 task 只能由它对应的 executor 执行，**不存在被其它 job 抢走**。这就在 OSS 上复刻了 JIT 的语义。

### 为什么是 facade，而不是让 Pod 直接连 Gitea

因为 task 被**worker 的身份**领取了，Pod 里的 runner 若直接连 Gitea：
- 它自己 `FetchTask` 只会拿到"等待中"的 job（已领取的不在其中）→ 拿不到手头这个 task；
- 用 worker 的凭据直接连 Gitea，则 Gitea 只看到一个 runner 身份在跑所有 job，且**凭据下发到了 Pod**。

facade 让 stock runner **不改一行**（`--instance` 指向 facade 即可），同时把凭据留在 controller。

## 5. 架构

单 Go 二进制，`controller` 子命令。组件：

```
Gitea ◀──(worker 用 registration 领 FetchTask / 回报)── controller ──建 pod──▶ executor Pod
                                                          │                     │
                                                          └── runner-facade ◀────┘（stock gitea-runner 走这里）
```

- **worker 池**：N 个 worker，每个 = 一条 Gitea runner registration（池标签）+ 一个本地槽位。有空位才 `FetchTask`；领到 task → 建 executor Pod。满载时 task 留在 Gitea 排队，等池内其它空位。
- **runner-facade**（每个 worker 一个）：实现 RunnerService（`Register`/`Declare`/`FetchTask`/`UpdateTask`/`UpdateLog`/`Ping`），**只**把预领取的 task 交给持有对应 capability 的 executor；其余调用用领取者身份转发到 Gitea。**一个 facade 只服务一个 worker 实例**。
- **executor Pod**：一次性，跑 stock `gitea-runner`（**host 模式**，在 Pod 内执行 workflow）。`--instance` 指向 facade。
- **生命周期 reconciler**：清理已结束的 Pod 与未上报终态就退出的 executor，释放槽位；超期未完成则把 task 报失败供 rerun。

## 6. 一个 job 的时序

1. worker `FetchTask` → 领到 task T（Gitea 侧 T 归属该 worker）。
2. controller 为 T 建 Pod：注入 **capability**（本 assignment 专用）、facade URL、池镜像与资源。
3. Pod 内 stock runner 向 facade `Register`/`Declare`（用 capability 认证；facade 返回合成 registration，**不写 Gitea**）。
4. runner `FetchTask` → facade 返回**已被领取的 T** → runner 在 Pod 内（host 模式）执行 workflow。
5. runner 的日志/状态 → facade → Gitea（领取者身份）。
6. 终态后：Pod 回收（TTL/删除）；worker 槽位释放，继续下一次 `FetchTask`。

注：Pod 里的 job 自身要访问**真实 Gitea**（git clone、artifacts、cache——task context 里带的是真实 Gitea 地址与 per-task token），所以 Pod 需要同时能到 facade 和真实 Gitea（都在集群内）。

## 7. 为什么 v1 不需要 webhook

`FetchTask` 本身就是触发器，worker 领取即感知到工作。因此 v1 **不需要** Gitea webhook、**不需要** `ALLOWED_HOST_LIST`、**不需要** repo webhook secret。将来若要"按队列深度扩缩容"或做观测，再加 `workflow_job` webhook（它是可选的增强，不是必需）。

## 8. 两种部署形态

同一二进制、同一镜像，靠配置/清单切换：

| | **用户级** | **集群级** |
| --- | --- | --- |
| 权限 | Role/RoleBinding（本 ns） | ClusterRole/ClusterRoleBinding |
| 目标 ns | 只能是自己的 `ns-xxx` | v1 先建到 controller 自己的 ns（后续再谈映射） |
| watch 范围 | 自己 ns | 全集群 |
| 部署者 | 用户（自己的 kubeconfig） | 集群管理员 |
| 上游凭据 | 自己的 Gitea registration token | 同上（按池配置） |

用户级形态的硬约束：**不依赖任何集群级资源**（无 CRD、无 ClusterRole、无跨 ns 权限）。

## 9. 配置（草案）

- `GITEA_INSTANCE_URL`：真实 Gitea 地址（worker 领任务 + job 访问用）。
- `*_REGISTRATION_TOKEN_FILE`：worker 的 registration（**只从文件读**，不进 Pod）。
- 池定义：`labels → { executor 镜像, resources, capacity }`。
- `SCOPE`：`namespace` | `cluster`；`TARGET_NAMESPACE`（集群级用）。
- `FACADE_LISTEN` / `FACADE_URL`：facade 的监听与下发到 Pod 的地址。
- `WORKER_COUNT`（默认待定）：并发槽位。
- 生命周期：Pod `ttlSecondsAfterFinished`、task 超时阈值。

## 10. 镜像与工件

- **controller 镜像**：本二进制。
- **runner 镜像**（executor Pod 用）：基础镜像需含 **git / node**（让 `actions/checkout@v4` 直接可用）+ **`gitea-runner` 二进制**；在 Pod 内以 host 模式执行。
- **工件仓库**：Gitea 内置容器 registry（已核实启用，`/v2/` 返回 401 registry 响应）。私有包需给集群配 `imagePullSecret`。
- **构建方式**：待定（见 §12）。

## 11. 未决：workflow 里构建镜像

kaniko **必须"作为自己的镜像运行"**（官方 Known Issues：不支持把它的二进制搬进别的镜像；构建时 `RUN` 跑在**你 Dockerfile 的基础镜像**里，不是 kaniko 镜像里）。所以"job Pod 里没有 docker daemon 又要构建镜像"需要单独设计，候选：

1. **build pool 的 Pod 镜像直接用官方 kaniko 镜像**（合规），workflow 用 `--context=git://…` 取源码（代价：不是 `actions/checkout` 标准写法；kaniko 镜像无 node）。
2. workflow 内经 k8s API 另起一个 kaniko Pod（可行但非标准，要额外 RBAC）。
3. 放弃 daemonless（DinD）——已否决。

→ **不在这版解决**，单列一轮。

## 12. 安全边界

- Gitea registration 只在 controller；**不下发到 Pod**。
- executor 只拿到**绑定本次 assignment 的 capability**，拿不到任何长期凭据。
- facade 只服务持有匹配 capability 的 executor；一个 facade 只对一个 worker 实例。
- RBAC 最小化：用户级 = a ns 内 pods 的 create/get/list/delete（+ 若走 k8s Pod 清理所需的最小项）。

## 13. 未决问题（需拍板）

1. **交接实现**：facade（不改官方 runner，**推荐**）vs fork/patch runner。
2. **Pod↔facade 鉴权**：每 assignment 一个 capability token（**推荐**，简单）vs SPIFFE mTLS。
3. **worker 的 registration 粒度**：repo 级 / user 级；注册流程（脚本？）。
4. **并发槽位默认值**与满载行为。
5. **runner 镜像**基础与构建流水线（CI 用哪套；推哪个 registry）。
6. **卡住 task 的兜底**：超时、释放、报失败供 rerun 的阈值与策略。
7. **仓库名**（GitHub private）与目录/模块名。
8. 是否把 `workflow_job` webhook 作为可选增强纳入 v0.2。

## 14. 里程碑

- **v0.1**：controller（worker + facade）+ executor Pod + 两种 scope 骨架；端到端跑通 `runs-on: ubuntu-latest` + `uses: actions/checkout@v4`。
- **v0.2**：并发/清理/观测、webhook 可选增强、卡住 task 兜底。
- **v0.3**：workflow 内构建镜像（§11）。

## 15. 验收标准

- `runs-on: ubuntu-latest`、`uses: actions/checkout@v4` 端到端成功。
- **每个 job 一个 Pod**，job 结束即销毁。
- **精确归属**：Gitea 里 task.runner_id 与执行它的 Pod 一一对应，无"被别的 job 抢走"。
- **用户级形态**：仅用 `ns-xxx` 的 kubeconfig + ns 级 RBAC 即可部署运行。
- 全程无 privileged、无 docker daemon。
