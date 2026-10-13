---
title: 兼容矩阵
slug: advanced/compatibility
weight: 42
---

# 与 GitHub / Gitea 的兼容性

判定标准：**除 Gitea 官方文档列出的差异外，其它都应当正常**（[Compared to GitHub Actions](https://docs.gitea.com/usage/actions/comparison)）。
本页分三类：可用的、属于文档差异的、**本平台做不到的（含原因）**。

## 一、可用（均在本仓库实测过）

`actions/checkout@v4`、`run`、`if`、`needs` + outputs、`strategy.matrix`、`env`、`github` 上下文、
`upload/download-artifact@v4`、`actions/cache@v4`（跨 job 命中需共享缓存服务）、`secrets`（日志打码）、`vars`、
`continue-on-error`、`timeout-minutes`、`concurrency` + `cancel-in-progress`、可复用 workflow（`workflow_call`）、
`rerun`、取消 job、多池并发、`workflow_dispatch`（含 inputs）。

在 workflow 里构建镜像：`docker login/build/push` 与官方 `docker/setup-qemu-action`、`setup-buildx-action`、
`login-action`、`metadata-action`、`build-push-action` 均可原样使用，见[在 workflow 里构建镜像](../usage/image-building.zh-cn.md)。

> 注意：Gitea 文档把 `concurrency` / `timeout-minutes` / `continue-on-error` 列在"忽略"里，**实测是生效的**
> （它们是 runner 侧实现，文档偏保守/过时）。`environment:` 被接受但无语义（Gitea 无该特性）。

## 二、属于"Gitea 与 GitHub 的差异"（照文档处理即可）

| 差异 | 影响 |
| --- | --- |
| `GITEA_TOKEN`（job 令牌）**不能**推 Gitea 的包/容器 registry | 推镜像要用 PAT 或账号密码做 `docker login`（本平台无关；这是 Gitea 侧限制，官方文档 + 上游 issue go-gitea/gitea#23642） |
| 容器包不会自动 link 到仓库 | 需要显式 `POST /api/v1/packages/{owner}/container/{name}/-/link/{repo}` |
| `hashFiles` 表达式返回空 | 用别的方式算 hash |
| `permissions:` 无 GitHub 专有 scope（`id-token`、`pages`、`checks`…） | 用到这些 scope 的 workflow 需调整 |
| `uses: docker://<image>` / `services:` / `container:` 在**无 docker daemon 的 runner** 上不可用 | Gitea 默认 runner 用 docker 模式时支持；本平台是 host 模式（见下），会**明确报错** |
| push 触发时 `inputs.*` 取值为空 | `if: inputs.x != false` 这类条件在 push 触发下两边平台都会判 false（已实测），要写 `github.event_name == 'push' \|\| inputs.x` |

## 三、本平台做不到的（不在文档差异里，必须知道）

| 缺口 | 原因 | 表现 |
| --- | --- | --- |
| `container:` / `services:` / `uses: docker://` | runner 以 **host 模式**运行且集群命名空间禁特权 → 没有 docker daemon | 领到 task 后**快速失败**并把原因写进 job 日志（不静默） |
| `docker run` / `docker compose` | 同上 | `docker` 替身明确报错 |
| **多平台构建**（`platforms: linux/amd64,linux/arm64`） | 需要 binfmt/QEMU 模拟，而注册模拟器要 `docker run --privileged`（安全基线不允许）；交叉编译也绕不过（需要 per-stage platform，kaniko 不支持；且 node 这类语言必须有真 arm64 执行环境） | 明确报错：`多平台构建（--platform=…）本平台做不到…` |
| `windows-*` / `macos-*` | runner 是 Linux 进程 | 没有 runner 能接 → 一直排队 |

多平台要支持只有两条前置（都在平台之外）：**节点上注册 binfmt**（装 qemu-user-static + systemd-binfmt，
建议带 `F` 标志，则容器内无需 qemu），或**加一台 arm64 节点**并把 arm64 构建调度过去；具备之后平台侧还要
把多平台展开成"逐平台各构建一次 + 合并 manifest list"。

## 四、行为上值得知道的差异

- **没有 runner 能接的 job 一直排队**（GitHub 是 24h 后自动失败），且 Gitea 没有 cancel run 的 API。
- **workflow 语法错是静默失败**：run 会被创建但零 job、零日志；先本地 parse 一遍 YAML。
- **自带 actions 的下载源**：Gitea 的 `DEFAULT_ACTIONS_URL` 默认指向 github（可用 `self` 指向自身）。
