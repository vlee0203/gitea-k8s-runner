---
title: 排查
slug: help/troubleshooting
weight: 50
---

# 排查：症状 → 原因

先看两个地方：**controller 日志**（`kubectl logs deploy/gitea-runner-controller`）与
**Gitea 的 job 日志**（用户在 UI 上看到的那份）。平台把"自己造成"的失败都写进了 job 日志（`##[error]`）。

| 症状 | 常见原因 | 怎么确认/解决 |
| --- | --- | --- |
| job 一直 `queued`，不动 | 没有 runner 声明它要的 label（拼写错、或该环境不存在）；或所有匹配 runner 满载 | `GET /api/v1/{user,admin}/actions/runners` 看 label 与 `status`/`busy`；对照[池与 runs-on](../usage/overview.zh-cn.md) |
| run 建了，但**零 job、零日志** | workflow **YAML 语法错**（Gitea 静默失败） | 本地先 `yaml` 解析一遍；常见坑是值没加引号而被 `: ` 截断 |
| job 跑到一半变 `cancelled`（构建类） | Pod 被**驱逐**：`ephemeralStorage` 超限（LimitRange 默认值很小） | `kubectl get events` 找 `Evicted ... exceeds ... ephemeral`；池里把 `resources.ephemeralStorage` 调大 |
| 改了镜像/代码，**行为没变** | 池镜像用的是可变 tag（如 `:ubuntu-latest`），拉取策略又非 Always，Pod 用的是节点缓存 | 平台 executor Pod 已强制 `PullAlways`；自建池镜像时注意同样问题 |
| 部署/滚动更新**卡住不完成** | controller 在 **drain**：等正在跑的 job 结束（上限 `SHUTDOWN_TIMEOUT`，默认 30m）；或有"起不来"的 Pod 占着槽位 | 看 `kubectl get pods` 有无 `Init:StartError`/`ImagePullBackOff`；后者现在会被判失败并自动删除。要"发布优先"就调小 `SHUTDOWN_TIMEOUT` |
| job 失败并写着 `executor 无法启动: …` | executor Pod 起不来（镜像名错、缺 Secret/ConfigMap、镜像没有 shell…） | 按日志里的原因修（例如 kaniko 必须用 `-debug` 变体，普通镜像没有 shell） |
| job 失败：`不支持 docker run` / `多平台构建…做不到` | 用了需要真 docker daemon 的写法 | 见[兼容矩阵](../advanced/compatibility.zh-cn.md)；镜像构建改用 `docker build/push` 或官方 docker action |
| Gitea 的 Runners 列表里堆着**离线条目** | 平台的 worker 记录轮换/池 label 改动后会留下旧记录；**平台刻意不自动删除**（会误伤别人的 runner） | 在对应作用域的 Runners 页面手工删，或按作用域调 `DELETE …/actions/runners/{id}` |
| `docker build` 成功但 `docker push` 报"没有可推送的镜像" | `push` 与 `build` 不在同一个 job（替身的构建参数记录在 Pod 内的临时文件里） | 把 build 与 push 放在同一个 job |
| job 里下载/构建/`uses:` 卡住或超时（外网要走代理的环境） | 代理没配到 Pod 上，或 `NO_PROXY` 把集群内地址也送进了代理 | controller 上设 `HTTP_PROXY/HTTPS_PROXY/NO_PROXY`（平台会注入 Pod 并自动追加集群内地址）；action 也可改从 Gitea 取（`[actions] DEFAULT_ACTIONS_URL = self`） |
| 注册失败 / worker 起不来 | 注册令牌失效（被重置）、Secret 名不对、或 state Secret 不可写 | 看 controller 日志首几行；注册令牌重置后要同步更新 Secret |

## 收集现场

```bash
# controller 侧
kubectl -n <ns> logs deploy/gitea-runner-controller --tail=200
kubectl -n <ns> get pods -l app.kubernetes.io/managed-by=gitea-runner-controller   # 在跑的 executor
kubectl -n <ns> get events --sort-by=.lastTimestamp | tail -30

# job 侧：Gitea UI 的 job 日志；或 API
#   GET /repos/{owner}/{repo}/actions/jobs/{job_id}/logs
```

executor Pod 的命名是 `grc-<repo>-<job>-<taskid>`，注解里有 repository / run-id / job / workflow，
`kubectl describe pod` 能直接对上是哪个 job。
