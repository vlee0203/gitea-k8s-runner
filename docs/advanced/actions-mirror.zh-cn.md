---
title: 平台公共 action（本地镜像）
slug: advanced/actions-mirror
weight: 44
---

# 平台公共 action

`uses:` 里的每个 action **就是一个独立的 git 仓库**：`uses: actions/checkout@v4` 的含义是"clone 该仓库、
切到 `v4`、执行其中的 `action.yml`"。这是 Actions 模型本身的规定（GitHub 同样"一个 action 一个仓库"，
没有"一个仓库装下所有 action"的写法），所以"平台公共 action"必然表现为**一组同名的公共仓库**。

本实例设了 `DEFAULT_ACTIONS_URL=self`（见[部署](../installation/overview.zh-cn.md)），不带主机的
`uses: <owner>/<repo>@<ref>` 会**从本实例解析**。为了让标准 GitHub 写法直接可用，已镜像：

| owner | 仓库 |
| --- | --- |
| `actions` | `checkout`、`cache`、`upload-artifact`、`download-artifact`、`setup-go`、`setup-node`、`setup-python`、`setup-java`、`setup-dotnet`、`github-script` |
| `docker` | `login-action`、`build-push-action`、`setup-qemu-action`、`setup-buildx-action`、`metadata-action` |

全部 **public**（所有用户的所有仓库都能用）；除个别例外都是 **pull mirror**，Gitea 会按间隔自动同步上游。

## 再加一个 action

1. **owner 必须与 `uses:` 里写的完全一致**（`uses: foo/bar@v1` → owner `foo`、仓库 `bar`）。
2. 优先用 migrate + mirror（保 tag、能自动同步）：

   ```
   POST /api/v1/repos/migrate
   {"clone_addr":"https://github.com/<owner>/<repo>.git",
    "repo_owner":"<owner>","repo_name":"<repo>","mirror":true,"auth_token":"<GitHub token>"}
   ```

3. 确认仓库里**有 workflow 会用到的 ref**（如 `v4`）——ref 不在，runner 就 clone 不到（报错形如
   `git clone --no-checkout https://<实例>/actions/setup-go … exit …`）。
4. 记进上面那张表。

## 已知例外

`actions/cache` 的上游仓库里有 Gitea 侧 `git fsck` 拒绝的对象（`largePathname: contains excessively
large pathname`），migrate 会失败 → 改用"本地裸克隆 + 只推 `refs/heads/v*`、`refs/tags/v*`"。
代价：**不是 mirror、不会自动同步**，上游更新要手工重推。

同类症状（迁移/推送 `unexpected disconnect`、`could not open '/var/lib/gitea/...'`）多半是仓库大或
Gitea 侧留了半成品目录：删掉该仓库重来，或改走上面这条"裸克隆 + 推 ref"的路。

## 注意

- 这些仓库是**平台级公共基础设施**：谁改了它们，等于往所有引用它的 job 里注入代码。建议 `actions` /
  `docker` 两个组织**只保留管理员**为成员。
- 单个步骤想用别的来源时，`uses:` 支持绝对 URL（`uses: https://github.com/actions/setup-go@v5`）以及
  `self:` / `builtin:` / `$/` 前缀——见 Gitea 官方的 Compared to GitHub Actions。
