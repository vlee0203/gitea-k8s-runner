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
  gitea-runner-controller/controller:latest \
  gitea-runner-controller/runner:ubuntu-latest \
  osscontainertools/kaniko:v1.28.6-debug
do
  src=${img%%:*}; tag=${img##*:}
  base=${src##*/}
  skopeo copy --all "docker://ghcr.io/$src:$tag" "docker://$REG/gitea-runner-controller/$base:$tag"
done
```

- `--all` 搬多架构；内网是 amd64 单架构也可以用 `--override-arch amd64` 少搬一半。
- 想钉住版本就用 workflow 产出的**不可变标签** `sha-<短 commit sha>`（见
  [开发文档](../advanced/development.zh-cn.md)）：`.../controller:sha-abc123…`，比 `latest` 可复现，
  内网 `skopeo copy` 也会连 tag 一起带过去。
- kaniko 的源在 `ghcr.io/osscontainertools/`，前两个在 `ghcr.io/vlee0203/` —— 上面的循环统一落到
  内网仓库的 `gitea-runner-controller/` 目录下，名字取原镜像名（`controller` / `runner` / `kaniko`）。
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
| `deploy/{namespace,cluster}/all.yaml` | `pools.yaml` 里两个池的 `image:` | `$REG/gitea-runner-controller/runner:ubuntu-latest` |
| 同上 | 容器 `image:` | `$REG/gitea-runner-controller/controller:latest` |
| 同上 | env `RUNNER_IMAGE` | `$REG/gitea-runner-controller/runner:ubuntu-latest` |
| 同上 | env `KANIKO_IMAGE` | `$REG/gitea-runner-controller/kaniko:v1.28.6-debug` |
| `deploy/cache.yaml`（可选） | 容器 `image:` | 同上 runner 镜像 |

```bash
# 一处漏改的后果：executor Pod 起不来（ImagePullBackOff），或 workflow 里 docker build 卡住找不到 kaniko
sed -i "s#ghcr.io/vlee0203/gitea-runner-controller#${REG}/gitea-runner-controller#g; s#ghcr.io/osscontainertools/kaniko#${REG}/gitea-runner-controller/kaniko#g" deploy/namespace/all.yaml
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

先把"谁真的会跟 Gitea 说 TLS"查清楚（核实到代码），因为它决定了要在哪儿装 CA：

| 谁 | 跟谁通信 | 要不要信任 Gitea 的证书 |
| --- | --- | --- |
| **controller** | **Gitea（https）**：注册 runner、facade 反向代理到真实 Gitea | **要** |
| 执行 Pod 里的 runner / `actions/checkout` | 集群内的 **facade（明文 http）** | 不要 |
| kaniko 旁路容器 | 构建上下文是**本地目录**（`dir://`），不连 Gitea；**但**workflow 若往自签的 Gitea registry 推镜像，它就要 | 推镜像时才要（由 `EXECUTOR_CA_PATH` 注入 CA 解决，不必烧镜像）|
| workflow 里直接访问 Gitea API 的步骤 | Gitea（https） | 要（同上，`EXECUTOR_CA_PATH` 注入即覆盖） |

依据：执行 Pod 里的 `CONTROLLER_URL` 指向 **controller 自己**（`internal/executor/executor.go` 里
`{Name: "CONTROLLER_URL", Value: s.ControllerURL}`，就是集群内的明文 Service），所以 Pod 侧取代码不走
Gitea；controller 侧全代码没有 `InsecureSkipVerify`，走标准证书校验。

### 一份 CA，两处用途

先把"需要哪些证书"定下来：客户端要能验通服务端证书的**签发链**，所以需要的是链上的 **中间证书 + 根 CA**；
**服务端自己的那张（leaf）不需要**（由服务端在握手中给出）。若内网服务端下发的链不完整，把中间证书一并放进
信任文件即可 —— 两份拼成一个 PEM 最省事（也可以各自一个 key，见下）。

**只建一个对象**，挂给 controller；执行 Pod 用的是**同一个挂载点** —— 不必为同一份证书再建第二个：

```bash
cat middle.crt root-ca.crt > ca-bundle.crt          # 顺序无所谓，都是 PEM 块
kubectl -n <应用命名空间> create configmap grc-ca --from-file=corp-ca.crt
```

ConfigMap 的形态（要手写就用这个框架，值自己填）：

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: grc-ca
  namespace: <应用命名空间>
data:
  corp-ca.crt: |            # key 名随你（就是 --from-file 用到的文件名），代码不认某个固定名字
    -----BEGIN CERTIFICATE-----
    <中间证书 middle>
    -----END CERTIFICATE-----
    -----BEGIN CERTIFICATE-----
    <根证书 ca>
    -----END CERTIFICATE-----
```

**① controller 自己得先信任它**（否则注册 runner、反代到 Gitea 都过不了 TLS）。加**一处 env + 一个卷 +
一个挂载**（三处，缺一不可）：

```yaml
          env:
            # Go 标准库同时扫这些目录；目录不存在也不影响公网证书（已实测正反对照）
            - name: SSL_CERT_DIR
              value: /etc/ssl/certs:/etc/grc-ca
          volumeMounts:
            - { name: grc-ca, mountPath: /etc/grc-ca, readOnly: true }
      volumes:
        - name: grc-ca
          # 不写 items：整个 ConfigMap 挂进来、目录下每个文件都会被扫到，key 名不必是 ca.crt
          configMap:
            name: grc-ca
```

**② 执行 Pod 也信任同一份**：把这**同一个挂载点（或其中某个证书文件）的路径**填进 `EXECUTOR_CA_PATH`
即可（清单里有注释行）。这一行只认路径 —— 挂的是 Secret 还是 ConfigMap、key 叫什么名字，它都不关心：

```yaml
            # - { name: EXECUTOR_CA_PATH, value: /etc/grc-ca }
            # - { name: EXECUTOR_CA_PATH, value: /etc/grc-ca/corp-ca.crt }   # 指单个文件也行
```

controller 建每个执行 Pod 时都会：

- 把读到的 CA 写进这条 assignment 自己的 Secret（所以 Pod 不引用上面那个对象，也就无所谓它是
  Secret 还是 ConfigMap、key 叫什么）
- 加一个 `ca-bundle` init 容器，把**镜像自带的根证书**与该 CA 拼成一个 bundle
- 给每个容器（runner 与 kaniko）挂载该 bundle，并把各工具链实际认的变量指过去：
  `SSL_CERT_FILE`（Go/kaniko、OpenSSL）、`SSL_CERT_DIR`（双保险）、`CURL_CA_BUNDLE`、`GIT_SSL_CAINFO`、
  `NODE_EXTRA_CA_CERTS`（JS action 的 fetch）、`REQUESTS_CA_BUNDLE`（python）

**注意是"拼接"而不是只指自定义 CA**：这些变量都是替换语义，只给自定义 CA 会把公网根证书顶掉，
Pod 里再访问任何公网 https 就废了。

**`EXECUTOR_CA_PATH` 是"配了就必须能读到"**：路径不存在、是空目录、或文件里没有 PEM，controller
**直接起不来**（日志里写清原因）—— 故意的，否则那份 CA 会一路静默不生效，只在 workflow 某一步炸 TLS。
所以别一边给这个卷写 `optional: true` 一边打开这一项。

改完重启（`kubectl rollout restart deploy/gitea-runner-controller`）。

> **双向 TLS（mTLS）另说**：如果 Gitea 要求客户端出示证书（而不只是你这边验服务端），当前 controller **不支持**
> ——它只做服务端校验，没有配客户端证书/私钥的入口。那种情况要改代码，告诉我再加。

### 什么时候才需要 ② 这一步

正常流量不需要：执行 Pod 全程只跟集群内的 facade 说明文 http（`CONTROLLER_URL` 就是集群内的 Service），
action 下载与 checkout 也走它。只有这两种情况要执行 Pod 自己信任 Gitea：

- workflow 往**自签的 Gitea registry 推镜像**（kaniko 容器）
- workflow 里**直接访问 Gitea 的 https**（例如直连 Gitea API，而不是用平台注入的地址）

排查时先分清是哪一侧报的：controller 侧报 = ① 的卷/`SSL_CERT_DIR` 没配；执行 Pod 侧报 = ② 没配，
或者 `EXECUTOR_CA_PATH` 指的那个路径与 ① 挂的不是同一份。

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
2. `kubectl -n <ns> logs deploy/gitea-runner-controller` —— 出现 `registered worker …` 与 `controller leading: …`
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
| `x509: certificate signed by unknown authority` | 第四节：Gitea 用私 CA。controller 报 = 它的卷/`SSL_CERT_DIR` 没配；执行 Pod 报（kaniko 推镜像 / workflow 直连 Gitea API）= `EXECUTOR_CA_PATH` 没配，或它指的路径与 ① 挂的不是同一份 |
| job 一直卡在 running、**一条日志都没有**，执行 Pod 也不自动删除 | 执行 Pod 的 runner 没能注册到 controller：`kubectl -n <ns> logs <pod> -c runner` 里是 `Invalid input … error="instance address is empty"`（无限重试）→ 这个池缺了上游 `run.sh` 注册要的 `GITEA_INSTANCE_URL`。**升级到 2026-10-16 之后的 controller 镜像**（`a860e90` 起两个名字都设，并给注册重试加了上界）；出现这类卡死先看这条日志，别等池的 `timeout` |
| Pod 被驱逐，`exceeded its local ephemeral storage limit` | 命名空间 LimitRange 的默认值太小，池里没显式给 `ephemeralStorage` |
| `cache` Deployment 一直 Pending | 没有可用 StorageClass（`cache.yaml` 是可选组件，直接跳过） |
| workflow 里 `docker build` 卡住/找不到 kaniko | `KANIKO_IMAGE` 没换成内网地址 |
| 构建报 `error checking push permissions … Get "https://https/v2/": EOF` | 镜像引用里带了协议（`docker build -t https://gitea…/img:tag`）：kaniko 把 `https` 当成了 registry 主机名。把 `REGISTRY`（或 secret）里的 `https://` 去掉；新 runner 镜像的 `docker` 替身会自动剥掉协议并告警 |

## 七、做成 Sealos 模板时的注意点

目标集群也是 Sealos 的话，把本文这些取值落成模板（`index.yaml`）时，注意这几条 —— 都是踩过的：

- **执行 Pod 与 controller 同命名空间**：`SCOPE=namespace`，`TARGET_NAMESPACE` 用 fieldRef 取自己，
  这样只需要该命名空间的 RBAC，不需要集群级权限。
- **服务名用模板变量**：`CONTROLLER_URL` 要指向本应用自己的 Service（`http://<app_name>:8080`）。
- **自定义 CA 不在模板里建**（模板不替环境做决定）：自签证书时按第四节加 —— controller 挂一份
  （`/etc/ssl/certs:/etc/grc-ca` 已验证：CA 在则自签证书被信任、CA 不在则公网证书照常可信），
  执行 Pod 侧只填 `EXECUTOR_CA_PATH` 指向**同一个挂载点**，模板里不建第二个对象。
- **执行 Pod 只在两种情况下需要 CA**（workflow 往自签 registry 推镜像 / 直接访问 Gitea 的 https）；
  正常流量全程只跟集群内的 facade 说明文 http。模板里两处都留注释，由部署时决定。
- **镜像**：模板里钉不可变引用（`sha-<sha>` 标签或摘要），别钉 `latest`；内网填内网仓库地址。
- **资源基线**：controller `limits 500m/1Gi`、`requests 50m/100Mi`，`revisionHistoryLimit: 1`，
  `imagePullPolicy: IfNotPresent`；controller 需要 k8s API，靠
  `automountServiceAccountToken: true`（用默认注入的 SA 令牌，不另配投射卷）。
- 打包成集群镜像：用 sealos-templates 仓库的 `scripts/build-cluster-image.sh <index.yaml> <镜像名>:<tag>`。

