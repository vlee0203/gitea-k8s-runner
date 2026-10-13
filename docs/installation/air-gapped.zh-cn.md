---
title: 内网（不出网）部署
slug: installation/air-gapped
weight: 12
---

# 内网（不出网）部署

目标集群 Pod **完全不能出网**时，有四样东西必须提前搬进去 —— 它们平时都是从公网现取的：

| # | 要搬的 | 平时从哪来 | 搬到哪 |
| --- | --- | --- | --- |
| 1 | `controller` 镜像 | `ghcr.io` | 内网镜像仓库 |
| 2 | `runner` 镜像（执行环境；cache-server 也用同一个） | `ghcr.io` | 内网镜像仓库 |
| 3 | `kaniko` 镜像 | `ghcr.io` | 内网镜像仓库 |
| 4 | **15 个 action 仓库** | `github.com`（或 Gitea 的 `self`） | 内网 Gitea |

第 4 项最容易漏：实例设了 `DEFAULT_ACTIONS_URL=self` 之后，`uses: actions/checkout@v4` 会去**你自己的
Gitea** 找 `actions/checkout`，找不到就直接失败，而且**不会回落 github.com**。详见
[平台公共 action](../advanced/actions-mirror.zh-cn.md)。

---

## 一、搬镜像

在任意一台**能出网、且装了 `skopeo`** 的机器上（比如你的公网 VPS）：

```bash
REG=<内网registry，如 harbor.corp.local/library>

# ghcr 上这两个是我们自己构建的私有包，先登录（PAT，read:packages 即可）
skopeo login ghcr.io -u <GitHub用户名>

for img in \
  gitea-k8s-runner/controller:latest \
  gitea-k8s-runner/runner:ubuntu-latest \
  osscontainertools/kaniko:v1.28.6-debug
do
  src=${img%%:*}; tag=${img##*:}
  base=${src##*/}
  skopeo copy --all "docker://ghcr.io/$src:$tag" "docker://$REG/gitea-k8s-runner/$base:$tag"
done
```

- `--all` 搬多架构；内网是 amd64 单架构也可以用 `--override-arch amd64` 少搬一半。
- kaniko 的源在 `ghcr.io/osscontainertools/`，前两个在 `ghcr.io/vlee0203/` —— 上面的循环统一落到
  内网仓库的 `gitea-k8s-runner/` 目录下，名字取原镜像名（`controller` / `runner` / `kaniko`）。
- 目标仓库要认证时：建一个拉取凭据，并把清单里**注释掉的** `imagePullSecrets` 与
  `IMAGE_PULL_SECRET` 打开（两处名字要一致）：

  ```bash
  kubectl -n <ns> create secret docker-registry registry-pull \
    --docker-server=$REG --docker-username=<user> --docker-password=<pass>
  ```

### 镜像地址要改的地方

搬完必须把清单/配置里的地址换掉，**四处池镜像 + 三处 controller 配置**：

| 文件 | 位置 | 换成 |
| --- | --- | --- |
| `deploy/{namespace,cluster}/all.yaml` | `pools.yaml` 里两个池的 `image:` | `$REG/gitea-k8s-runner/runner:ubuntu-latest` |
| 同上 | 容器 `image:` | `$REG/gitea-k8s-runner/controller:latest` |
| 同上 | env `RUNNER_IMAGE` | `$REG/gitea-k8s-runner/runner:ubuntu-latest` |
| 同上 | env `KANIKO_IMAGE` | `$REG/gitea-k8s-runner/kaniko:v1.28.6-debug` |
| `deploy/cache.yaml`（可选） | 容器 `image:` | 同上 runner 镜像 |

```bash
# 一处漏改的后果：executor Pod 起不来（ImagePullBackOff），或 workflow 里 docker build 卡住找不到 kaniko
sed -i "s#ghcr.io/vlee0203/gitea-k8s-runner#${REG}/gitea-k8s-runner#g; s#ghcr.io/osscontainertools/kaniko#${REG}/gitea-k8s-runner/kaniko#g" deploy/namespace/all.yaml
```

## 二、搬 action

用仓库里的 `scripts/mirror-actions.sh`，分两段（这样只需要一台机器能出网，不必是同一台）：

```bash
# ① 在能出网、能访问 github.com 的机器上打包（有 HTTP(S)_PROXY 会自动走）
./scripts/mirror-actions.sh export --out actions-bundle.tar     # 15 个 action，体积百 MB 级

# ② 把 tar 带进内网，在能同时访问 tar 和内网 Gitea 的机器上导入
./scripts/mirror-actions.sh import \
  --gitea http://10.0.0.10:3000 --user admin --token <管理员 PAT> --bundle actions-bundle.tar
```

- 导入会建组织（`actions` / `docker`）、建仓库、推 `refs/heads/v*` 与 `refs/tags/v*`。
  这些都需要**管理员**权限（目标实例通常已关掉普通用户建组织）。
- 幂等：目标仓库已有 tags 就跳过；要强制重推翻加 `--repush`。
- 只补一个 action（比如以后要用 `actions/setup-ruby`）：两条命令都加 `--only actions/setup-ruby`
  （该仓库需要先加进脚本顶部的 `ACTIONS` 列表）。
- 只搬了 `v*`：workflow 里 `uses: xxx@master` 这类非 `v*` 的 ref 拿不到。

## 三、目标 Gitea 的配置

```bash
kubectl -n <gitea-ns> set env sts/<gitea> \
  GITEA__actions__DEFAULT_ACTIONS_URL=self \
  GITEA__service__DISABLE_REGISTRATION=true \
  GITEA__openid__ENABLE_OPENID_SIGNUP=false \
  GITEA__admin__DISABLE_REGULAR_ORG_CREATION=true
```

前一项让 action 从自己实例取（内网必需）；后三项是**必须一起做的收口**：`self` 模式下
`uses: <owner>/<repo>@<ref>` 的解析权在自己实例，而上游 owner 名在 Gitea 侧没有保护，
不关注册/建组织的话，任何人都能抢注 `aws-actions` 这类还没被占的名字来投毒。理由与实测见
[平台公共 action](../advanced/actions-mirror.zh-cn.md)。

别忘了**注册令牌**（作用域决定这个 controller 服务谁，内网一般用站点级）：

```
管理后台 → 站点管理 → Actions → Runners → 创建 / 取注册令牌
# 或 POST /admin/actions/runners/registration-token
```

## 四、HTTPS 与证书（内网最容易踩）

**优先用 http。** 内网 Gitea 常见形态是裸 IP + 无 TLS，直接：

```
GITEA_INSTANCE_URL = http://10.0.0.10:3000
```

这样执行环境里的 `git` / `node` / kaniko 都不会遇到证书问题。

若必须 HTTPS + 自签或企业 CA，注意**执行 Pod 目前无法注入自定义 CA** —— 池的字段只有
`labels/image/resources/slots/timeout`，没有额外的 env 或卷。两条可行路：

1. **把 CA 烧进 runner 镜像**（一次性，推荐）：构建 runner 镜像时加两行，之后所有 Pod 都认这个 CA。

   ```dockerfile
   # container/runner.Dockerfile 里追加
   COPY corp-root-ca.crt /usr/local/share/ca-certificates/corp-root-ca.crt
   RUN update-ca-certificates
   ```

   内网 Gitea 的证书换了要重新构建这个镜像。
2. 只用 http 访问 Gitea、HTTPS 留给别的服务。

只给 controller 设 `SSL_CERT_FILE` 是不够的：它只解决 controller 自己与 Gitea 通信，
executor Pod 里的 git 照样会失败。

### 还有一类"要能拉到的东西"：构建的基础镜像

workflow 里 `docker build` 的 `FROM` 引用的基础镜像，是**执行 Pod 自己**去拉（kaniko 直接连 registry），
内网同样拉不到公网镜像。两种做法：把基础镜像也搬进内网仓库、Dockerfile 里 `FROM <内网registry>/…`；
或者让 kaniko 走 registry mirror —— 后者平台还没有暴露参数（要的话在 kaniko 容器命令行上加
`--registry-mirror`）。用内网 base 镜像的 CI 不受影响。

## 五、集群前置条件

| 项 | 要求 | 不满足会怎样 |
| --- | --- | --- |
| k8s 版本 | 实测 1.28；清单只用 `v1` 与 `coordination.k8s.io/v1`，1.24+ 应可用 | 老版本 API 缺失 |
| PodSecurity | `baseline` 或更宽松即可（Pod 不跑特权、不挂宿主路径） | `restricted` 命名空间会**拒绝**创建 Pod（清单没设 `runAsNonRoot` / `seccompProfile` 等） |
| LimitRange | 命名空间若有默认 `ephemeral-storage` 上限（如 100Mi），**池的 `resources.ephemeralStorage` 必须显式给足**（清单里是 10Gi） | executor 被驱逐：`exceeded its local ephemeral storage limit` |
| 节点架构 | runner 镜像是 **amd64** | arm64 节点上镜像跑不起来 |
| StorageClass | **仅** `cache.yaml`（可选组件）要 PVC；没有默认 StorageClass 就跳过这一步 | cache Deployment 一直 Pending（不影响主流程） |

`executor Pod` 与 `controller` 建在同一个命名空间（`TARGET_NAMESPACE`），该命名空间不需要任何
出网能力，只要能访问内网 Gitea 与内网镜像仓库。

## 六、部署与校验

部署步骤与 `deploy/` 各字段的说明见[部署总览](overview.zh-cn.md)，内网额外确认这几点：

1. `kubectl -n <ns> get pods` —— controller `Running`；**没有** `ImagePullBackOff`（有就是镜像地址没改全）
2. `kubectl -n <ns> logs deploy/gitea-k8s-runner` —— 出现 `registered worker …` 与 `controller leading: …`
3. 内网 Gitea 的 Runners 页面能看到 runner 且 `online`
4. **跑一个标准的 GitHub workflow 验收**（这一步同时验证 action 搬运）：

   ```yaml
   name: smoke
   on: push
   jobs:
     t:
       runs-on: ubuntu-latest
       steps:
         - uses: actions/checkout@v4     # ← 命中内网镜像的 action 才算搬成功
         - run: echo ok
   ```

   失败形态对照：`git clone --no-checkout http://<gitea>/actions/checkout` 报错 = action 没搬或
   搬的 ref 不合适；`ImagePullBackOff` = 第 2 项的镜像没搬；证书报错 = 第四节。

## 附：内网翻车点速查

| 症状 | 原因 |
| --- | --- |
| job 秒失败，报 `git clone … /<owner>/<repo>` | 目标实例缺这个 action 仓库（第二节没搬，或没搬你要用的 ref） |
| `ImagePullBackOff` / `ErrImagePull` | 镜像没搬进内网仓库，或清单里地址没改全，或私有仓库缺 pull secret |
| `x509: certificate signed by unknown authority` | 第四节：Gitea 用了私 CA，而 Pod 不认 |
| Pod 被驱逐，`exceeded its local ephemeral storage limit` | 命名空间 LimitRange 的默认值太小，池里没显式给 `ephemeralStorage` |
| `cache` Deployment 一直 Pending | 没有可用 StorageClass（`cache.yaml` 是可选组件，直接跳过） |
| workflow 里 `docker build` 卡住/找不到 kaniko | `KANIKO_IMAGE` 没换成内网地址 |
