# gitea-k8s-runner

把 Gitea Actions 的 job 跑成 Kubernetes 里**一次性 Pod** 的 controller（类 GitHub ARC 的形态）：

1. controller 里的 worker 以自身 runner 身份**先原子领取**一个 task（Gitea 侧归属当场确定，别的 worker 抢不走）；
2. 为这个 task 建一个一次性 executor Pod，Pod 内的 **stock `gitea-runner`** 通过 controller 的 **facade** 拿到
   "这个已被领取的 task"并执行；
3. 日志/状态经 facade 用领取者身份回传 Gitea，跑完删除 Pod。

**一个 job = 一个 Pod**，环境互不污染。

为什么自建：Gitea OSS 没有 JIT configuration API（服务端无法"把指定 job 绑给指定 runner"），官方 runner 的
`kubernetes://` 模式要求 k8s ≥ 1.30。机制细节、取舍与支持矩阵见 `DESIGN.md`（不入库）。

## 部署形态

同一个二进制/镜像，**差别只在注册令牌的作用域**（令牌作用域决定这个 controller 服务谁）：

| 形态 | 注册令牌 | 服务范围 | 清单 |
| --- | --- | --- | --- |
| 用户级（自助、最小 RBAC） | user 级 | 该用户名下所有仓 | `deploy/namespace/all.yaml` |
| 站点级（平台自带 runner） | site 级 | **全站所有仓**，等价 GitHub-hosted | `deploy/cluster/all.yaml` |
| 仓库级（最小权限） | repo 级 | 仅该仓 | 同上，换令牌 |

令牌一律放 k8s Secret（key `token`），controller 经 **k8s API 读取**（`REGISTRATION_TOKEN_SECRET`），
**不落盘、不随 Pod 分发**。

## 部署

```bash
# 1. 造令牌：repo/user 级在对应 Settings → Actions → Runners；site 级在站点管理后台
kubectl -n <ns> create secret generic gitea-runner-registration --from-literal=token=<TOKEN>

# 2. 按需改清单里的 pools.yaml（labels / image / resources / slots）与
#    GITEA_INSTANCE_URL、FACADE_URL、RUNNER_IMAGE
# 3. 应用
kubectl apply -f deploy/namespace/all.yaml    # 或 deploy/cluster/all.yaml
```

可选的共享缓存（跨 job 命中 `actions/cache`，否则每个一次性 Pod 都是冷缓存）：`kubectl apply -f deploy/cache.yaml`。

## 池（pool）与 runs-on

一个池 = 一组 `labels` + 镜像 + 资源 + 并发槽数（`slots`；满载时任务留在 Gitea 队列等空位）。

Gitea 的匹配规则是**子集**（`models/actions/runner.go:202 CanMatchLabels`）：job 的 `runs-on` 里**每一个**
label，runner 都必须声明过。因此：

- 一个池挂一串 label 就能覆盖一串 runs-on；拆池的理由是 **image / resources / slots 不同**，不是 label 数量。
- **label 是承诺**：声明 `ubuntu-24.04` 就得真有 24.04 的镜像，否则 workflow 不会报错，只会静默地按错误环境跑。
- 示例的通用池挂 GitHub 托管 runner 那套别名 `[self-hosted, linux, x64, ubuntu-latest, ubuntu-24.04]`，
  标准 workflow 零改动命中；`linux`/`x64` 只挂通用池，否则 `runs-on: [self-hosted, linux]` 会落到 kaniko 池上。
- `windows-*` / `macos-*` 覆盖不了（runner 本质是 Linux 进程）。
- 没有 runner 能接的 job 会**一直 queued 且不报错**（GitHub 是排队 24h 后自动失败）——写错 `runs-on` 的表现就是"卡住"。

## 运维要点

- **controller 不需要任何 Gitea API token**，只持有注册令牌这一条凭据。
- **注册令牌长期有效**：Gitea 的 REST API 不能旋转它（`shared.GetRegistrationToken` 只返回现值），
  泄漏后没有自动收场手段——只能在后台点「重置注册令牌」人工作废。别让它落盘、别贴进聊天。
- workflow 语法错在 Gitea 上是**静默失败**：run 被创建但零 job、零日志；排查时先本地 parse 一遍 YAML。
- executor Pod 名 `gitea-runner-<repo>-<job>-<taskid>`，带 `gitea-k8s-runner/assignment` label；
  `kubectl get pods` 能直接看出在跑哪个仓、哪个 job。
- 不支持：`container:`、`services:`、`uses: docker://`（无 docker daemon）。构建镜像走 kaniko 池
  （`injectRunner: true`：Pod 主容器仍是官方 kaniko 镜像，runner 二进制由 initContainer 注入）。

## 镜像与 CI

| 文件 | 内容 |
| --- | --- |
| `container/controller.Dockerfile` | 编译 controller，跑在 distroless 上 |
| `container/runner.Dockerfile` | 通用执行环境（工具链 + stock `gitea-runner`） |

`.github/workflows/build.yaml`（push 到 main / 手动触发）推
`ghcr.io/vlee0203/gitea-k8s-runner/{controller:latest,runner:ubuntu-latest}`。
