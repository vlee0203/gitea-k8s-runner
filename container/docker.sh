#!/bin/sh
# docker —— 本平台的 docker CLI 替身：把标准 docker 用法翻译成 kaniko。
#
# 为什么需要它：本平台的 Pod 跑在 PSA baseline 的命名空间里（不许特权容器），
# docker / podman / buildah 都要"挂载文件系统"，没有 CAP_SYS_ADMIN 就干不了；
# 只有 kaniko 不挂载，能在这种限制下构建镜像。于是这里提供一个同名 `docker`，
# 让 workflow 里的 `docker login` / `docker build` / `docker push` 一个字都不用改。
#
# 覆盖：login、build、buildx build、push、tag、version、info。
# 不覆盖（需要真 daemon，明确报错而不是静默失败）：run、compose、pull、exec、save、manifest …
set -eu

KANIKO=${KANIKO_PATH:-/kaniko/executor}
SPEC=/tmp/.gkr-last-build          # 最近一次 build 的参数，push 时复用
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
    ""|*,*) [ -z "$GKR_PLATFORM" ] || die "多平台构建（--platform=$GKR_PLATFORM）本平台做不到：kaniko 一次只能构建一个平台"
            GKR_PLATFORM= ;;
  esac
  # 记下这次构建：kaniko 没有本地镜像库，push 时只能按同样的参数再构建一次
  printf '%s\n%s\n%s\n%s\n%s\n' "$GKR_CTX" "$GKR_FILE" "$GKR_ARGS" "$GKR_TARGET" "$GKR_PLATFORM" > "$SPEC"
}

# 用当前 GKR_* 跑一次 kaniko；GKR_DEST 非空则构建并推送，否则只构建
run_kaniko() {
  args="--context=dir://$GKR_CTX --dockerfile=$GKR_FILE"
  for a in $GKR_ARGS; do args="$args --build-arg $a"; done
  [ -z "$GKR_TARGET" ]   || args="$args --target $GKR_TARGET"
  [ -z "$GKR_PLATFORM" ] || args="$args --custom-platform $GKR_PLATFORM"
  if [ -n "${GKR_DEST:-}" ]; then
    for d in $GKR_DEST; do args="$args --destination=$d"; done
  else
    args="$args --no-push"
  fi
  log "kaniko $args"
  # shellcheck disable=SC2086
  exec "$KANIKO" --verbosity=info $args
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
    mkdir -p /kaniko/.docker
    auth=$(printf '%s:%s' "$user" "$pass" | base64 | tr -d '\n')
    printf '{"auths":{"%s":{"auth":"%s"}}}' "$host" "$auth" > /kaniko/.docker/config.json
    log "凭据已写入 /kaniko/.docker/config.json（$host）"
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
      GKR_DEST="$GKR_TAGS" run_kaniko   # 多个 tag 一次构建、推多份
      exit 0
    fi
    GKR_DEST="" run_kaniko
    ;;

  push)
    ref=${1:-}; [ -n "$ref" ] || die "push 需要镜像名：docker push <ref>"
    [ -f "$SPEC" ] || die "没有可推送的镜像：本平台没有本地镜像库，push 只能跟在同一个 job 里的 build 之后"
    { read -r GKR_CTX <&3; read -r GKR_FILE <&3; read -r GKR_ARGS <&3; read -r GKR_TARGET <&3; read -r GKR_PLATFORM <&3; } 3< "$SPEC" || true
    # kaniko 一次可推多个 destination：如果 ref 是上次 build 的某个 tag，就把那批 tag 一起推掉
    # （`docker build -t a -t b .` + `docker push a` 是常见写法，不该为此构建两遍）。
    case " ${GKR_TAGS:-} " in
      *" $ref "*) GKR_DEST="$GKR_TAGS" ;;
      *)          GKR_DEST="$GKR_TAGS $ref" ;;
    esac
    log "push $ref：kaniko 没有本地镜像库，按上次 build 的参数重新构建并推送（destination:$GKR_DEST）"
    run_kaniko
    ;;

  tag)   log "tag 已忽略（构建时直接打目标 tag）" ;;
  version|--version) echo "Docker version 27.0.0 (gitea-k8s-runner kaniko shim)" ;;
  info)  echo "Server: kaniko (no docker daemon in this platform; images are built with kaniko)" ;;
  "")    die "用法：docker {login|build|push|tag|version} ..." ;;
  *)     die "本平台没有 docker daemon，不支持 docker $cmd（可用：login / build / buildx build / push / tag）" ;;
esac
