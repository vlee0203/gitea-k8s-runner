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

# 解析 build 的参数（$@ = build 之后的所有参数），结果放进 GKR_* 变量
parse_build() {
  GKR_TAGS=""; GKR_FILE=""; GKR_CTX=""; GKR_ARGS=""; GKR_TARGET=""; GKR_PLATFORM=""
  GKR_PUSH=0; GKR_IGNORED=""
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
      --iidfile=*|--metadata-file=*|--cache-from=*|--cache-to=*|--secret=*|--ssh=*|--output=*|--builder=*|--network=*|--shm-size=*|--label=*|--progress=*|--provenance=*|--sbom=*|--attest=*)
                       GKR_IGNORED="$GKR_IGNORED ${1%%=*}"; shift ;;
      --push)       GKR_PUSH=1; shift ;;
      # —— 带值、但 kaniko 无对应物的参数：连值一起吃掉，只记一笔 ——
      --iidfile|--metadata-file|--cache-from|--cache-to|--secret|--ssh|--output|-o|--builder|--network|--shm-size|--label)
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
  [ -n "$GKR_FILE" ] || GKR_FILE="$GKR_CTX/Dockerfile"
  [ -z "$GKR_IGNORED" ] || log "忽略 kaniko 无对应物的参数：$GKR_IGNORED"
  case "$GKR_PLATFORM" in
    *", "*) die "多平台构建（--platform=$GKR_PLATFORM）本平台做不到：kaniko 一次只能构建一个平台" ;;
  esac
  printf '%s\n%s\n%s\n%s\n%s\n%s\n' "$GKR_CTX" "$GKR_FILE" "$GKR_ARGS" "$GKR_TARGET" "$GKR_PLATFORM" "$GKR_TAGS" > "$SPEC"
}

# 把一次构建（GKR_* + GKR_DEST）交给 kaniko 容器，并把它的输出流回来
submit() {
  [ -d "$SHARED" ] || die "这个 job 里没有 kaniko 容器（$SHARED 不存在）：请让平台把该 job 识别为构建任务"
  dir="$SHARED/req/$$-$(date +%s)"
  mkdir -p "$dir"
  : > "$dir/args"
  printf '%s\n' "--context=dir://$GKR_CTX" "--dockerfile=$GKR_FILE" >> "$dir/args"
  for a in $GKR_ARGS; do printf '%s\n' "--build-arg" "$a" >> "$dir/args"; done
  [ -z "$GKR_TARGET" ]   || printf '%s\n' "--target" "$GKR_TARGET" >> "$dir/args"
  [ -z "$GKR_PLATFORM" ] || printf '%s\n' "--custom-platform" "$GKR_PLATFORM" >> "$dir/args"
  if [ -n "${GKR_DEST:-}" ]; then
    for d in $GKR_DEST; do printf '%s\n' "--destination=$d" >> "$dir/args"; done
  else
    printf '%s\n' "--no-push" >> "$dir/args"
  fi
  printf '%s\n' "--verbosity=info" >> "$dir/args"
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
    mkdir -p "$(dirname "$CREDS")"
    auth=$(printf '%s:%s' "$user" "$pass" | base64 | tr -d '\n')
    printf '{"auths":{"%s":{"auth":"%s"}}}' "$host" "$auth" > "$CREDS"
    log "凭据已写入 $CREDS（kaniko 容器读同一份）"
    ;;

  build|buildx)
    if [ "$cmd" = buildx ]; then
      sub=${1:-}; [ $# -gt 0 ] && shift || true
      case "$sub" in
        build)  : ;;
        version) echo "github.com/docker/buildx v0.17.1 (kaniko shim)"; exit 0 ;;
        create|use|ls|inspect|rm|stop|prune)
                 log "buildx $sub：本平台没有 buildx builder，已忽略（构建由 kaniko 完成）"; exit 0 ;;
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
  version|--version) echo "Docker version 27.0.0 (gitea-k8s-runner kaniko shim)" ;;
  info)  echo "Server: kaniko (no docker daemon in this platform; the pod runs a kaniko container)" ;;
  "")    die "用法：docker {login|build|push|tag|version} ..." ;;
  *)     die "本平台没有 docker daemon，不支持 docker $cmd（可用：login / build / buildx build / push / tag）" ;;
esac
