---
title: 安全模型
slug: advanced/security
weight: 41
---

# 安全模型

一句话：**作用域隔离 + 平台只持一条注册令牌**；泄漏的代价被明确接受，不用"缩小作用域"当挡箭牌。

## 1. 作用域隔离（这是地基）

Gitea 的 runner 模型是"**一条注册令牌的作用域决定这个 runner 能服务谁**"，没有更细的授权。所以：

- 平台自带 runner 注册在**自己的作用域**（推荐站点级：服务全站所有仓，等价 GitHub-hosted）。
- 用户自己注册的 runner 在各自的 **user / repo 作用域**，两边清单天然分开——平台**看不见、也不会去动**。

于是"某个用户的令牌泄漏"**只影响该用户自己**，不波及平台；平台只需保证自己那条令牌不泄漏。

## 2. 平台只持有注册令牌这一条凭据

| 措施 | 说明 |
| --- | --- |
| 不落盘 | 令牌放在 k8s Secret（`REGISTRATION_TOKEN_SECRET`），controller 在**注册那一刻**经 k8s API 读取；Pod 文件系统里没有它 |
| 不下发 | 注册令牌不会进入 executor Pod；Pod 只拿一次性 capability |
| executor 侧一次性 | 每个 assignment 一张 capability（随机、绑定该 assignment、随 Pod 销毁），泄漏影响限于那一个 job |
| 不需要额外凭据 | controller **不需要**任何 Gitea API token（不巡检、不删 runner、不读 job 列表） |

## 3. 刻意**不做**的事（不是遗漏）

**不做 runner 巡检删除。** 曾经实现过"定期扫本作用域的 runner，删掉不是自己注册的"，已删除，原因：

- Gitea 的 runner 列表 API **不暴露作用域**（只返回 id/name/labels/status…），runner 记录也**不记注册者**；
- 因此它**区分不了"攻击者用泄漏令牌注册的 runner"和"别人合法注册的 runner"**——在共享作用域里扫删必然误伤，
  站点级部署下更会把全站用户的 runner 删光；
- 对照 GitHub：GitHub 也从不删除用户自己注册的 self-hosted runner。

**明确接受的一件事**：注册令牌一旦泄漏，**没有自动收尾手段**。Gitea 的 REST API 只能*返回*当前注册令牌、
不能旋转（源码 `routers/api/v1/shared/runners.go` 走 `GetOrCreateActiveRunnerToken`；模型层的
`NewRunnerTokenWithValue` 能作废旧令牌但没暴露成 API）。Web 界面**有**"重置注册令牌"入口
（`POST …/runners/reset_registration_token`，站点/用户/组织/仓库四级共用），所以**人工轮换可行、API 自动化不可行**。

对照 GitHub：GitHub 的注册令牌 **1 小时过期**，所以它能承受"不巡检"；Gitea 的令牌长期有效——把安全押在
"不泄漏"上（不落盘 + 只在注册瞬间读 + 最小权限），这是 Gitea 生态下的取舍。

## 4. 运维建议

- 定期**人工**在后台点一次「重置注册令牌」（站点级/用户级各一次），轮换周期按你的风险偏好定；
  重置后同步更新 Secret，否则注册会失败。
- 令牌**不要**贴进聊天、日志、镜像或 CI 变量；出现过的令牌按"已泄漏"处理（重置）。
- 巡检类需求用**只读**手段做（Gitea 侧看 Runners 页面 / 只读 admin token），不要让平台持有能改的凭据。
