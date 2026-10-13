---
title: 文档
slug: docs
weight: 10
---

# gitea-k8s-runner 文档

把 Gitea Actions 的 job 跑成 Kubernetes 里**一次性 Pod** 的 controller（类 GitHub ARC）。
workflow 以 **GitHub Actions 标准**为准：`runs-on: ubuntu-latest`、`uses: actions/checkout@v4`、
`docker/build-push-action@v6` 这类写法原样可用。

页面文件名按语言分兄弟文件（`<page>.zh-cn.md` / `<page>.en-us.md`），目前只有中文。

## 章节

| 章节 | 页面 | 讲什么 | 什么时候看 |
| --- | --- | --- | --- |
| [安装](installation/overview.zh-cn.md) | 部署总览 | 三种作用域形态、前置条件、逐步部署、部署后校验 | 第一次部署 / 换形态 |
| [用法](usage/overview.zh-cn.md) | 写 workflow | 标准写法示例、池与 `runs-on` 的匹配规则 | 写/改 CI |
| [用法](usage/image-building.zh-cn.md) | 在 workflow 里构建镜像 | `docker build/push` 与官方 docker action 怎么被接住、边界在哪 | CI 里要出镜像 |
| [进阶](advanced/architecture.zh-cn.md) | 架构与机制 | 先领取 + facade 交接、一次性 Pod、状态恢复、单例与 drain | 想改代码 / 排查深层问题 |
| [进阶](advanced/security.zh-cn.md) | 安全模型 | 作用域隔离、平台只持注册令牌、**刻意不做**的那些事 | 安全评审 |
| [进阶](advanced/compatibility.zh-cn.md) | 兼容矩阵 | 哪些 GitHub 能力可用、哪些是 Gitea 文档差异、哪些本平台做不到及原因 | 迁移既有 workflow |
| [帮助](help/troubleshooting.zh-cn.md) | 排查 | 症状 → 原因（静默失败、被驱逐、卡住、改了没生效…） | 出问题 |

## 相关

- 仓库根 [README](../README.md)：一句话定位、镜像、构建方式（速查）。
- `deploy/`：`namespace/all.yaml`（用户级）、`cluster/all.yaml`（集群级）、`cache.yaml`（可选共享缓存）。
- `container/docker.sh`：本平台的 `docker` 替身（把标准 docker 命令转给同 Pod 的 kaniko 容器）。
