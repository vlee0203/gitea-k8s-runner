#!/bin/sh
# docker —— 本平台的 docker CLI 替身：把标准 docker 命令转给同一个 Pod 里的 kaniko 容器。
#
# 为什么不自己构建：本平台的 Pod 跑在 PSA baseline 的命名空间里（不许特权容器），
# docker / podman / buildah 都要"挂载文件系统"，没有 CAP_SYS_ADMIN 干不了；能用的只有 kaniko。
# 而 kaniko 官方明确要求**作为它自己的镜像运行**（README: "kaniko is meant to be run as an
# image … We do not recommend running the kaniko executor binary in another image"）——
# 它会清空自己容器的根文件系统来解包基础镜像，挤在别的容器里会把别人的东西一起删掉。
#
# 所以：要构建镜像的 job，Pod 里会多一个 **官方 kaniko 镜像的容器**（无特权、无 daemon、
# 无 socket，只是待命），本替身通过共享目录把构建请求交给它，再把它的日志流回 job 日志。
# 共享目录与请求协议见 internal/executor（sidecar 侧）与本文件末尾（客户端侧）。
#
# 覆盖：login、build、buildx build、push、tag、version、info。
# 不覆盖（需要真 daemon，明确报错而不是静默失败）：run、compose、pull、exec、save、manifest …
set -eu

SHARED=${GKR_SHARED:-/shared}          # 与 kaniko 容器共享的目录（由 controller 挂载）
CREDS="$SHARED/kaniko/.docker/config.json"
SPEC=/tmp/.gkr-last-build              # 最近一次 build 的参数，push 时复用
log() { echo "[docker] $*" >&2; }
die() { log "$*"; exit 1; }

# 注册表地址带协议（`https://gitea.corp.local`）是非常常见的写法 —— docker 自己会报
# invalid reference format，而 kaniko 更阴：它把 "https" 当成 registry 主机名，报
# `error checking push permissions … Get "https://https/v2/": EOF`（实测踩过）。
# 这里统一剥掉协议并说一声：镜像引用与凭据里的键都只写主机名。
strip_scheme() {
  case "$1" in
    *://*) log "镜像引用不该带协议，已去掉：$1 -> ${1#*://}"; printf '%s' "${1#*://}" ;;
    *)     printf '%s' "$1" ;;
  esac
}

# 解析 build 的参数（$@ = build 之后的所有参数），结果放进 GKR_* 变量
parse_build() {
  GKR_TAGS=""; GKR_FILE=""; GKR_CTX=""; GKR_ARGS=""; GKR_TARGET=""; GKR_PLATFORM=""
  GKR_PUSH=0; GKR_IGNORED=""; GKR_IID=""; GKR_META=""
  while [ $# -gt 0 ]; do
    case "$1" in
      -t|--tag)     GKR_TAGS="$GKR_TAGS $2"; shift 2 ;;
      -f|--file)    GKR_FILE=$2; shift 2 ;;
      --build-arg)  GKR_ARGS="$GKR_ARGS $2"; shift 2 ;;
      --target)     GKR_TARGET=$2; shift 2 ;;
      --platform)   GKR_PLATFORM=$2; shift 2 ;;
      # —— 等号形式：--flag=value ——
      --tag=*)         GKR_TAGS="$GKR_TAGS ${1#*=}"; shift ;;
      --file=*)        GKR_FILE=${1#*=}; shift ;;
      --build-arg=*)   GKR_ARGS="$GKR_ARGS ${1#*=}"; shift ;;
      --target=*)      GKR_TARGET=${1#*=}; shift ;;
      --platform=*)    GKR_PLATFORM=${1#*=}; shift ;;
      --iidfile=*)      GKR_IID=${1#*=}; shift ;;
      --metadata-file=*) GKR_META=${1#*=}; shift ;;
      --cache-from=*|--cache-to=*|--secret=*|--ssh=*|--output=*|--builder=*|--network=*|--shm-size=*|--label=*|--progress=*|--provenance=*|--sbom=*|--attest=*)
                       GKR_IGNORED="$GKR_IGNORED ${1%%=*}"; shift ;;
      --push)       GKR_PUSH=1; shift ;;
      # —— 带值、但 kaniko 无对应物的参数：连值一起吃掉，只记一笔 ——
      --iidfile)    GKR_IID=$2; shift 2 ;;
      --metadata-file) GKR_META=$2; shift 2 ;;
      --cache-from|--cache-to|--secret|--ssh|--output|-o|--builder|--network|--shm-size|--label)
                    GKR_IGNORED="$GKR_IGNORED $1"; shift 2 ;;
      # —— 布尔、无对应物的参数 ——
      --progress|--provenance|--sbom|--attest|--no-cache|--pull|--quiet|-q|--load|--compress)
                    GKR_IGNORED="$GKR_IGNORED $1"; shift ;;
      --)           shift; [ $# -gt 0 ] && GKR_CTX=$1 && shift ;;
      -*)           GKR_IGNORED="$GKR_IGNORED $1"; shift ;;
      *)            GKR_CTX=$1; shift ;;
    esac
  done
  [ -n "$GKR_CTX" ] || GKR_CTX=.
  # 构建请求是交给**另一个容器**执行的，相对路径在那里没有意义 → 一律转绝对路径
  GKR_CTX=$(cd "$GKR_CTX" && pwd)
  [ -n "$GKR_FILE" ] || GKR_FILE="$GKR_CTX/Dockerfile"
  [ -z "$GKR_IGNORED" ] || log "忽略 kaniko 无对应物的参数：$GKR_IGNORED"
  case "$GKR_PLATFORM" in
    *,*) die "多平台构建（--platform=$GKR_PLATFORM）本平台做不到：kaniko 一次只能构建一个平台（多平台需要 binfmt/QEMU 模拟，本集群安全基线不允许特权）" ;;
  esac
  printf '%s\n%s\n%s\n%s\n%s\n%s\n' "$GKR_CTX" "$GKR_FILE" "$GKR_ARGS" "$GKR_TARGET" "$GKR_PLATFORM" "$GKR_TAGS" > "$SPEC"
}

# 把一次构建（GKR_* + GKR_DEST）交给 kaniko 容器，并把它的输出流回来
submit() {
  [ -d "$SHARED" ] || die "这个 job 里没有 kaniko 容器（$SHARED 不存在）：请让平台把该 job 识别为构建任务"
  dir="$SHARED/req/$$-$(date +%s)"
  meta=""; iid=""
  for a in ${GKR_META:+"$GKR_META"}; do meta=$a; done
  [ -z "${GKR_META:-}" ] || meta=$GKR_META
  [ -z "${GKR_IID:-}" ] || iid=$GKR_IID
  mkdir -p "$dir"
  : > "$dir/args"
  printf '%s\n' "--context=dir://$GKR_CTX" "--dockerfile=$GKR_FILE" >> "$dir/args"
  for a in $GKR_ARGS; do printf '%s\n' "--build-arg" "$a" >> "$dir/args"; done
  [ -z "$GKR_TARGET" ]   || printf '%s\n' "--target" "$GKR_TARGET" >> "$dir/args"
  [ -z "$GKR_PLATFORM" ] || printf '%s\n' "--custom-platform" "$GKR_PLATFORM" >> "$dir/args"
  if [ -n "${GKR_DEST:-}" ]; then
    # 引用里带 http:// 就是"这台 registry 是 80、走明文" —— 剥掉协议的同时必须把它告诉 kaniko。
    # 为什么不靠 kaniko 自己判：go-containerregistry 只在私有网段/localhost/loopback/.local 才降级
    # 成 http，其余一律 https；所以 http:// 的公共域名（内网常见的 gitea.corp.local 之类）不显式
    # 指定就会用 https 去连。https:// 与不写协议同为默认（443），只需剥掉协议。
    insecure=""
    for d in $GKR_DEST; do
      case "$d" in
        http://*) h=${d#*://}; insecure="$insecure ${h%%/*}" ;;
      esac
      printf '%s\n' "--destination=$(strip_scheme "$d")" >> "$dir/args"
    done
    seen=""
    for h in $insecure; do
      case " $seen " in *" $h "*) continue ;; esac
      seen="$seen $h"
      printf '%s\n' "--insecure-registry=$h" >> "$dir/args"
    done
  else
    printf '%s\n' "--no-push" >> "$dir/args"
  fi
  printf '%s\n' "--verbosity=info" >> "$dir/args"
  # 把本次步骤看到的代理设置一并交给 kaniko 容器（构建走的是另一个容器，只拿到平台注入的那份；
  # workflow / 仓库 secret 里自带代理时，不转交的话拉基础镜像与推镜像会绕开它）。
  # NO_PROXY 采用"用户那份 + 平台那份"的合并，避免用户漏写集群内地址而把内网流量送进代理。
  : > "$dir/env"
  for pair in "HTTP_PROXY:${HTTP_PROXY:-}" "http_proxy:${http_proxy:-}" "HTTPS_PROXY:${HTTPS_PROXY:-}" "https_proxy:${https_proxy:-}"; do
    k=${pair%%:*}; v=${pair#*:}
    [ -n "$v" ] && printf '%s=%s\n' "$k" "$v" >> "$dir/env"
  done
  merged_no=$(printf '%s' "${NO_PROXY:-${no_proxy:-}}")
  if [ -n "${GKR_PLATFORM_NO_PROXY:-}" ]; then
    merged_no=$(printf '%s' "${merged_no:+$merged_no,}${GKR_PLATFORM_NO_PROXY}")
  fi
  [ -n "$merged_no" ] && printf 'NO_PROXY=%s\nno_proxy=%s\n' "$merged_no" "$merged_no" >> "$dir/env"
  log "转交代理设置给 kaniko 容器：$(tr '\n' ' ' < "$dir/env")"
  # 上面那份 env 只作用于 kaniko 自己（拉基础镜像、推镜像）。**构建步骤里的 RUN** 要出网
  # （go mod download / npm ci / pip install …）得另走一路：kaniko 不会把它的环境带进 RUN
  # （实测：给 kaniko 设了 HTTPS_PROXY，RUN 里 `env | grep -i _proxy` 什么都没有），
  # 而 `--build-arg HTTP(S)_PROXY/NO_PROXY` 会进构建环境（实测，Dockerfile 不用声明 ARG）——
  # 真 docker 也正是"自动带这几个"。所以照做。
  for name in HTTP_PROXY http_proxy HTTPS_PROXY https_proxy; do
    eval "v=\${$name:-}"
    [ -n "$v" ] && printf '%s\n' "--build-arg=$name=$v" >> "$dir/args"
  done
  [ -n "$merged_no" ] && printf '%s\n' "--build-arg=NO_PROXY=$merged_no" "--build-arg=no_proxy=$merged_no" >> "$dir/args"
  : > "$dir/go"                       # 准备就绪，kaniko 容器可以开工
  log "已交给 kaniko 容器：$(tr '\n' ' ' < "$dir/args")"

  # 边等边把 kaniko 的输出流到 job 日志（1 秒粒度，不重复打印）
  seen=0
  while [ ! -f "$dir/done" ]; do
    if [ -f "$dir/log" ]; then
      n=$(wc -c < "$dir/log")
      if [ "$n" -gt "$seen" ]; then tail -c +$((seen + 1)) "$dir/log"; seen=$n; fi
    fi
    sleep 1
  done
  if [ -f "$dir/log" ]; then
    n=$(wc -c < "$dir/log"); [ "$n" -gt "$seen" ] && tail -c +$((seen + 1)) "$dir/log"
  fi
  rc=$(cat "$dir/rc" 2>/dev/null || echo 1)
  # build-push-action 会读 --metadata-file / --iidfile 来算它的 outputs（digest 等）：
  # kaniko 的日志里有 "Pushed …@sha256:…"，据此写一份同样的元数据。
  digest=$(grep -oE '@sha256:[0-9a-f]{64}' "$dir/log" 2>/dev/null | head -1 | tr -d '@')
  tags=$(printf '%s' "${GKR_DEST:-${GKR_TAGS:-}}" | tr ' ' ',' | sed 's/^,*//; s/,*$//')
  if [ -n "$digest" ]; then
    [ -z "$iid" ] || printf '%s' "$digest" > "$iid"
    [ -z "$meta" ] || printf '{"containerimage.digest":"%s","containerimage.config.digest":"%s","image.name":"%s"}' \
      "$digest" "$digest" "$tags" > "$meta"
  fi
  rm -rf "$dir"
  exit "$rc"
}

cmd=${1:-}; [ $# -gt 0 ] && shift || true

case "$cmd" in
  login)
    host=""; user=""; pass=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -u|--username)    user=$2; shift 2 ;;
        -p|--password)    pass=$2; shift 2 ;;
        --password-stdin) pass=$(cat); shift ;;
        -*)               shift ;;
        *)                [ -n "$host" ] || host=$1; shift ;;
      esac
    done
    [ -n "$host" ] || die "login 需要 registry 地址：docker login <host> -u <user> -p <pass>"
    # config.json 的键要与 kaniko 查找时用的主机名一致，带协议会匹配不上凭据
    host=$(strip_scheme "$host")
    mkdir -p "$(dirname "$CREDS")"
    auth=$(printf '%s:%s' "$user" "$pass" | base64 | tr -d '\n')
    printf '{"auths":{"%s":{"auth":"%s"}}}' "$host" "$auth" > "$CREDS"
    log "凭据已写入 $CREDS（kaniko 容器读同一份）"
    ;;

  build|buildx)
    if [ "$cmd" = buildx ]; then
      sub=${1:-}; [ $# -gt 0 ] && shift || true
      case "$sub" in
        # 裸 `docker buildx`（无子命令）：build-push-action 的 isAvailable 就是跑这个并看退出码
        "")      printf 'github.com/docker/buildx v0.17.1 (kaniko shim)\n\nUsage: docker buildx [OPTIONS] COMMAND\n' ; exit 0 ;;
        build)  : ;;
        create|use|ls|rm|stop|prune)
                 log "buildx $sub：本平台没有 buildx builder（构建由 kaniko 完成）"; exit 0 ;;
        version) echo "github.com/docker/buildx v0.17.1 (kaniko shim)"; exit 0 ;;
        inspect)
                 # setup-buildx-action 按行解析这张表（键: 值），并读每个 node 的 endpoint。
                 name=default; while [ $# -gt 0 ]; do case "$1" in --builder) shift; name=$1; shift ;; *) shift ;; esac; done
                 printf 'Name:          %s\nDriver:        docker-container\n\nNodes:\nName:          %s0\nEndpoint:      unix:///var/run/docker.sock\nStatus:        running\nBuildkit:      v0.17.1\nPlatforms:     linux/amd64\n' "$name" "$name"
                 exit 0 ;;
        *)       die "不支持的 buildx 子命令：$sub" ;;
      esac
    fi
    parse_build "$@"
    if [ "$GKR_PUSH" = 1 ] && [ -n "$GKR_TAGS" ]; then
      GKR_DEST="$GKR_TAGS" submit       # 多个 tag 一次构建、推多份
    fi
    GKR_DEST="" submit
    ;;

  push)
    ref=${1:-}; [ -n "$ref" ] || die "push 需要镜像名：docker push <ref>"
    [ -f "$SPEC" ] || die "没有可推送的镜像：本平台没有本地镜像库，push 只能跟在同一个 job 里的 build 之后"
    { read -r GKR_CTX <&3; read -r GKR_FILE <&3; read -r GKR_ARGS <&3; read -r GKR_TARGET <&3; read -r GKR_PLATFORM <&3; read -r GKR_TAGS <&3; } 3< "$SPEC" || true
    # 如果 ref 是上次 build 的某个 tag，就把那批 tag 一起推掉（kaniko 一次可推多个 destination）
    case " ${GKR_TAGS:-} " in
      *" $ref "*) GKR_DEST="$GKR_TAGS" ;;
      *)          GKR_DEST="$GKR_TAGS $ref" ;;
    esac
    log "push $ref：本平台没有本地镜像库，按上次 build 的参数重新构建并推送"
    submit
    ;;

  tag)   log "tag 已忽略（构建时直接打目标 tag）" ;;
  logout) log "logout 已忽略（本平台凭据只写在共享目录里的 kaniko config，job 结束随 Pod 一起消失）" ;;
  version|--version)
    case " $* " in
      *--format*) echo "27.0.0" ;;                    # action 里常见：--format '{{.Client.Version}}'
      *) echo "Client: Docker Engine - Community (gitea-k8s-runner kaniko shim)\nServer: kaniko (no docker daemon)" ;;
    esac ;;
  info)
    case " $* " in
      *--format*) echo "27.0.0" ;;
      *) echo "Server: kaniko (no docker daemon in this platform; builds run in the pod's kaniko container)" ;;
    esac ;;

  # 官方 setup-qemu-action 会 pull binfmt 镜像并 run 它注册模拟器。本平台不做多平台模拟
  # （需要 binfmt/qemu → 需要特权，baseline 不许），所以这两步明确跳过并告警，不当成失败。
  pull)  log "跳过 docker pull $*：本平台没有 docker daemon，镜像拉取由 kaniko 构建时完成" ;;

  # 只读探测类命令：action 常用来取镜像 id / 列表 / 上下文。本平台没有本地镜像库，
  # 给一个形状正确的空/占位结果，让它们不至于因此中断（它们本来也只是探测）。
  images|image)
    sub=${1:-}; [ $# -gt 0 ] && shift || true
    case "$sub" in
      ""|ls|list) echo '[]' ;;          # `docker images` / `docker image ls`
      inspect)
        ref=${1:-unknown}
        printf '[{"Id":"sha256:%064d","RepoTags":["%s"],"Architecture":"amd64","Os":"linux","Config":{}}]' 0 "$ref" ;;
      *) die "不支持的 docker image 子命令：$sub" ;;
    esac ;;
  inspect)
    ref=${1:-unknown}
    printf '[{"Id":"sha256:%064d","RepoTags":["%s"],"Architecture":"amd64","Os":"linux","Config":{}}]' 0 "$ref" ;;
  context)
    sub=${1:-}; [ $# -gt 0 ] && shift || true
    case "$sub" in
      show) echo default ;;
      inspect)
        case " $* " in
          *--format*) echo default ;;      # 常见：--format '{{.Name}}'
          *) printf '[{"Name":"default","Metadata":{},"Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock","SkipTLSVerify":false}}}]' ;;
        esac ;;
      ls)   printf '{"currentContext":"default","contexts":[{"Name":"default","Current":true}]}' ;;
      *)    echo default ;;
    esac ;;
  run)
    case " $* " in
      *binfmt*|*qemu*|*tonistiigi/binfmt*)
        # setup-qemu-action 会把这条命令的**输出当 JSON 解析**（取已注册的模拟器列表），
        # 所以这里不能只打日志：给一份形状正确的结果；同时明确告警说模拟器并未真正注册
        # （多平台构建在本集群结构上做不到：binfmt/QEMU 需要特权，安全基线不允许）。
        log "跳过 docker run $*：本平台不做多平台模拟构建（binfmt/QEMU 需要特权，安全基线不允许）"
        printf '{"supported":["arm","arm64","amd64"],"emulators":{"arm":{"path":"/usr/bin/qemu-arm"},"arm64":{"path":"/usr/bin/qemu-aarch64"}}}' ;;
      *)
        die "本平台没有 docker daemon，不支持 docker run（只有 binfmt/QEMU 这类注册步骤会被跳过）" ;;
    esac ;;
  "")    printf 'Usage: docker [OPTIONS] COMMAND\n\n(docker shim for this platform: builds run in the pod'"'"'s kaniko container)\nAvailable: login, build, buildx build, push, tag, image inspect, images, context, version, info\n' ;;
  *)     die "本平台没有 docker daemon，不支持 docker $cmd（可用：login / build / buildx build / push / tag）" ;;
esac
