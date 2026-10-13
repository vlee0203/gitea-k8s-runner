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

全部 **public**（所有用户的所有仓库都能用）；除下节列的 4 个例外都是 **pull mirror**，Gitea 会按间隔自动同步上游。

这 15 个都经实测：临时分支上四个 job（go / node / python / script）全绿，覆盖 `checkout@v4`、
`setup-go@v5`、`setup-node@v4`、`setup-python@v5`、`github-script@v7` 五个 action。

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

## 已知例外（4 个：普通仓库，不自动同步）

`actions/cache`、`actions/setup-java`、`docker/build-push-action`、`docker/metadata-action` 四个的 migrate
在 Gitea 侧卡住（`git fsck` 拒绝：`largePathname: contains excessively large pathname`，或整仓超时），
→ 改用"**本地裸克隆 + 只推 `refs/heads/v*`、`refs/tags/v*`**"。
代价：**不是 mirror、不会自动同步**，上游更新要手工重推。

同类症状（迁移/推送 `unexpected disconnect`、`could not open '/var/lib/gitea/...'`）多半是仓库大或
Gitea 侧留了半成品目录：删掉该仓库重来，或改走上面这条"裸克隆 + 推 ref"的路。

## 注意：这是一条供应链，用之前先读

`DEFAULT_ACTIONS_URL=self` 的代价不只是"要自己镜像"。`uses: <owner>/<repo>@<ref>` 会被解析到**本实例**
（实测：未镜像的 action 报 `git clone --no-checkout https://<实例>/actions/setup-go`，不会回落 GitHub）。
而上游的 owner 名在 Gitea 侧**没有任何保护**：

- 组织名与用户名**同一个命名空间**（源码 `IsUserExist` 查同一张 `user` 表；实测 `GET /users/actions`
  返回的就是那个组织，`type: Organization`）→ 所以**已占的名字别人抢不走**（`actions`/`docker` 是安全的）。
- 但**没人抢的名字，谁先注册谁的**。源码 `reservedUsernames` 只挡 Gitea 自己的路由名
  （`api`/`org`/`user`/`login`/…），没有 `RESERVED_USERNAMES` 这类配置项；用户名字符集
  `^[\da-zA-Z][-.\w]*$` **允许连字符** → 抢注连组织都不用建，注册个普通用户名 `aws-actions` 就行。
- 本实例 `DISABLE_REGISTRATION = false`（还带 `ENABLE_CAPTCHA = false`、`REGISTER_EMAIL_CONFIRM = false`）、
  `DEFAULT_ALLOW_CREATE_ORGANIZATION = true` → **任何人都能自助注册，并建组织**。
- 于是：别人写 `uses: aws-actions/configure-aws-credentials@v4`，被抢注后拉到的就是攻击者的代码，且是在
  **受害者自己的 job 里、拿着他的 token 与 secrets** 执行。

**结论**：用 `self` 就等于把"名字解析"的权威交给自己实例。要收口，把下面这几条一起做：

| 项 | 位置 | 作用 |
| --- | --- | --- |
| `DISABLE_REGISTRATION = true`（连带 `ENABLE_OPENID_SIGNUP`） | `[service]` / `[openid]` | 断掉"陌生人自助注册→抢注用户名"这条路 |
| `DISABLE_REGULAR_ORG_CREATION = true` | `[admin]` | 非管理员一律不能建组织（源码 `CanCreateOrganization()`：`IsAdmin \|\| (AllowCreateOrganization && !DisableRegularOrgCreation)`） |
| `DEFAULT_ACTIONS_URL` 改回 `github` | `[actions]` | **唯一根治**：名字解析回 github.com，实例上谁注册什么都不影响；代价是 action 要走外网 |

若必须不出外网（`self` 得留着），那前两条是必须项。另外：**这两个组织的所有者现在是普通用户**（见下），
平台安全不该挂在一个普通账号的凭据上——把管理员加进 `actions`/`docker` 的 Owners 团队（或转移组织所有者），
并保证**没有任何非管理员在 Owners 里**：谁改了这些仓库，等于往所有引用它的 job 里注入代码。

单个步骤想用别的来源时，`uses:` 支持绝对 URL（`uses: https://github.com/actions/setup-go@v5`）以及
`self:` / `builtin:` / `$/` 前缀——见 Gitea 官方的 Compared to GitHub Actions。
