---
title: 写 workflow
slug: usage/overview
weight: 30
---

# 写 workflow

按 **GitHub Actions 标准**写就行，不需要为本平台做任何特殊配置：

```yaml
name: ci
on: [push, workflow_dispatch]
jobs:
  test:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        go: ['1.26']
    steps:
      - uses: actions/checkout@v4
      - run: go test ./...
      - uses: actions/upload-artifact@v4
        with: { name: logs, path: '*.log' }
```

在 workflow 里构建并推送镜像也是标准写法，见[在 workflow 里构建镜像](image-building.zh-cn.md)。

## 池（pool）

一个池 = **一组 `labels` + 镜像 + 资源 + 并发槽数**（`slots`）。controller 为每个 job 建一个一次性
Pod，用的就是这个池的镜像与资源。

```yaml
- labels: [self-hosted, linux, x64, ubuntu-latest, ubuntu-24.04]
  image: ghcr.io/vlee0203/gitea-k8s-runner/runner:ubuntu-latest
  resources: { cpu: "2", memory: 4Gi, ephemeralStorage: 10Gi }
  slots: 2          # 这个池最多同时跑几个 job；满了就留在 Gitea 队列等位
  timeout: 1h       # 单个 job 的执行上限（Pod 的 activeDeadlineSeconds）
```

## `runs-on` 是怎么匹配到池的

Gitea 的规则是**子集**（`models/actions/runner.go:202 CanMatchLabels`）：job 的 `runs-on` 里
**每一个** label，runner 都必须声明过。

```
池 A 声明: [self-hosted, linux, x64, ubuntu-latest, ubuntu-24.04]
  runs-on: ubuntu-latest                  ✅ 命中
  runs-on: [self-hosted, linux]           ✅ 命中
  runs-on: [ubuntu-latest, gpu]           ❌ 池没声明 gpu
  runs-on: windows-latest                 ❌ Linux runner 承载不了
```

由此三条实践：

- **一个池挂一串 label 就能覆盖一串 runs-on**，不需要为每个 `runs-on` 单独建池；拆池的理由是
  **image / resources / slots 不同**，不是 label 数量。
- **label 是承诺，不是装饰**：声明 `ubuntu-24.04` 就得真有 24.04 的镜像（本仓库的 runner 镜像基底
  经核实是 Ubuntu 24.04，所以只声明 `ubuntu-24.04`，不声明 `ubuntu-22.04`）。声明了不匹配的环境，
  workflow **不会报错**，只会静默地按错误环境跑。
- 通用别名（`linux`/`x64`）**只挂通用池**，否则 `runs-on: [self-hosted, linux]` 这类通用写法会落到
  专用池（槽位更少）上。

想对齐 GitHub 托管 runner 的体验，通用池可以声明
`[self-hosted, linux, x64, ubuntu-latest, ubuntu-24.04]`——标准 workflow 零改动命中。

## 覆盖不到的 `runs-on`

`windows-*` / `macos-*` 声明了也没用：runner 本质是 Linux 进程，Windows/macOS 的 workflow 拉到
Linux 上只会失败（Gitea/Actions 生态的固有边界）。

## 没有 runner 能接的 job 会怎样

**一直排队、不报错也不过期**（GitHub 是排队 24h 后自动失败，Gitea 不会；Gitea 也没有 cancel run 的
API）。所以写错 `runs-on` 的表现是"job 卡住不动"，先检查 label 拼写与池配置。

## 常用能力对照

| 能力 | 状态 |
| --- | --- |
| `actions/checkout@v4`、`actions/upload/download-artifact@v4`、`actions/cache@v4` | ✅（cache 跨 job 命中需要共享缓存服务器，见[部署](../installation/overview.zh-cn.md)） |
| `matrix`、`needs` + outputs、`if`、`env`、`github` 上下文、`secrets`/`vars` | ✅ |
| `concurrency` + `cancel-in-progress`、`timeout-minutes`、`continue-on-error` | ✅（Gitea 文档把这几条列为"忽略"，实测**是生效的**，属 runner 侧实现） |
| `container:` / `services:` / `uses: docker://` | ❌ 需要真 docker daemon，见[兼容矩阵](../advanced/compatibility.zh-cn.md) |
| 在 workflow 里 `docker build` / 官方 docker action | ✅ 见[在 workflow 里构建镜像](image-building.zh-cn.md) |
