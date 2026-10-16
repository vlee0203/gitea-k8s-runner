#!/usr/bin/env bash
# 把「平台公共 action」搬到另一个 Gitea 实例。
#
# 为什么需要：实例设了 DEFAULT_ACTIONS_URL=self 时，`uses: actions/checkout@v4` 会解析到
# **你自己的 Gitea**（不会回落 github.com）。目标实例上如果没有这些仓库，job 会直接失败。
#
# 两种用法：
#   export  —— 在能访问 github.com 的机器上，把各 action 的 v* ref 拉成一个 bundle（tar）
#   import  —— 把 bundle（或直接从 github）推到目标 Gitea
#
# 内网（完全不出网）就 export 一次、把 tar 带进内网，再 import：
#   ./mirror-actions.sh export --out actions-bundle.tar
#   ./mirror-actions.sh import --gitea http://10.0.0.10:3000 --user admin --token <PAT> --bundle actions-bundle.tar
#
# 目标实例能出网也可以省掉 bundle，直接 import（会现拉 github）。
# 只加一个 action：两条命令都加 --only actions/setup-go
# 目标仓库已存在且已有 tags 时 import 会跳过推送（幂等）；要强制重推翻加 --repush
#
# 依赖：bash、git、curl —— 不需要 jq / python。
set -euo pipefail

# 与 docs/advanced/actions-mirror.zh-cn.md 的表保持一致。owner 必须与 uses: 里写的一模一样。
ACTIONS=(
  actions/checkout
  actions/cache
  actions/upload-artifact
  actions/download-artifact
  actions/setup-go
  actions/setup-node
  actions/setup-python
  actions/setup-java
  actions/setup-dotnet
  actions/github-script
  actions/stale
  actions/labeler
  actions/delete-package-versions
  docker/login-action
  docker/build-push-action
  docker/setup-qemu-action
  docker/setup-buildx-action
  docker/metadata-action
  docker/bake-action
  # 第三方但用得极多（注意：镜像它们等于信任其上游仓库）
  ruby/setup-ruby
  shivammathur/setup-php
  softprops/action-gh-release
  peter-evans/create-pull-request
)

# --only owner/repo[,owner/repo…]：只处理列出的
ONLY=()
selected() {
  [ "${#ONLY[@]}" -eq 0 ] && return 0
  local x; for x in "${ONLY[@]}"; do [ "$x" = "$1" ] && return 0; done
  return 1
}

REPUSH=false                 # 目标仓库已有 tags 时默认跳过推送（幂等）；--repush 强制重推
INSECURE=false               # --insecure：跳过 TLS 校验（git 与 curl 都跟着跳），自签实例且懒得带 CA 时用
CURL=""                      # --insecure 时变成 -k
TMP=""                       # 全局，供 EXIT trap 清理
cleanup() { [ -n "$TMP" ] && rm -rf "$TMP"; return 0; }
trap cleanup EXIT

say()  { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }
die()  { printf '错误：%s\n' "$*" >&2; exit 1; }

usage() { sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }

# 走代理拉 github（有 HTTP(S)_PROXY 就用）
git_clone() {  # $1=owner/repo  $2=目标目录
  local spec="$1" dest="$2"
  local -a px=()
  [ -n "${HTTPS_PROXY:-}" ] && px+=(-c "https.proxy=$HTTPS_PROXY")
  [ -n "${HTTP_PROXY:-}"  ] && px+=(-c "http.proxy=$HTTP_PROXY")
  local try
  for try in 1 2 3; do
    if GIT_TERMINAL_PROMPT=0 git $GV "${px[@]}" clone --bare --quiet "$SRC_BASE/$spec.git" "$dest"; then
      return 0
    fi
    rm -rf "$dest"; sleep 5
  done
  return 1
}

# 用 Authorization 头带凭据 push：既不把密码写进 URL（会出现在 ps 里），也不用改写目标 scheme
git_push() {  # $1=本地裸库 $2=Gitea 基址(http/https) $3=owner/repo  $4=user  $5=token
  local auth; auth="$(printf '%s:%s' "$4" "$5" | base64 | tr -d '\n')"
  git -C "$1" $GV -c "http.extraHeader=Authorization: Basic $auth" push -q \
    "$2/$3.git" 'refs/heads/v*:refs/heads/v*' 'refs/tags/v*:refs/tags/v*'
}

SRC_BASE="${SRC_BASE:-https://github.com}"

set_insecure() {   # 由 --insecure 决定 git/curl 的跳过校验参数
  if [ "$INSECURE" = true ]; then GV="-c http.sslVerify=false"; CURL="-k"; else GV=""; CURL=""; fi
}

cmd_export() {
  local out=actions-bundle.tar
  while [ $# -gt 0 ]; do
    case "$1" in
      --out)  out="$2"; shift 2 ;;
      --from) SRC_BASE="$2"; shift 2 ;;
      --insecure) INSECURE=true; shift ;;   # 从已镜像好的实例打包（匿名可克隆即可），绕开 github
      --only) IFS=, read -r -a _x <<<"$2"; ONLY+=("${_x[@]}"); shift 2 ;;
      *) die "export 不认识参数：$1" ;;
    esac
  done
  set_insecure
  TMP="$(mktemp -d)"
  say "拉取 action 到 $TMP"
  for spec in "${ACTIONS[@]}"; do
    selected "$spec" || continue
    say "  $spec"
    # 只要 workflow 用得上的 v* ref：上游有些仓库带历史大对象，全量推会被 Gitea 的
    # git fsck 拒掉（见 docs/advanced/actions-mirror.zh-cn.md 的例外一节）。
    git_clone "$spec" "$TMP/$(echo "$spec" | tr / _).git" ||
      die "clone 失败：$spec（检查网络/代理）"
  done
  tar -C "$TMP" -cf "$out" .
  say "bundle 写好了：$out（$(du -h "$out" | cut -f1)）"
}

cmd_import() {
  local gitea="" user="" token="" bundle=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --gitea)  gitea="${2%/}"; shift 2 ;;
      --user)   user="$2"; shift 2 ;;
      --token)  token="$2"; shift 2 ;;
      --bundle) bundle="$2"; shift 2 ;;
      --only)   IFS=, read -r -a _x <<<"$2"; ONLY+=("${_x[@]}"); shift 2 ;;
      --repush) REPUSH=true; shift ;;
      --insecure) INSECURE=true; shift ;;
      *) die "import 不认识参数：$1" ;;
    esac
  done
  [ -n "$gitea" ] && [ -n "$user" ] && [ -n "$token" ] ||
    die "import 需要 --gitea <URL> --user <用户名> --token <PAT>"
  [ -n "$bundle" ] && [ ! -f "$bundle" ] && die "找不到 bundle：$bundle"
  case "$gitea" in http://*|https://*) ;; *) die "--gitea 要带 http:// 或 https://" ;; esac

  # 建组织/仓库需要管理员权限：目标实例通常会关掉普通用户建组织。
  set_insecure
  local orgs="" o spec org repo dir code
  for spec in "${ACTIONS[@]}"; do
    selected "$spec" || continue
    o="${spec%%/*}"
    case " $orgs " in *" $o "*) ;; *) orgs="$orgs $o" ;; esac
  done
  set_insecure
  TMP="$(mktemp -d)"
  if [ -n "$bundle" ]; then
    say "解开 bundle：$bundle"
    tar -C "$TMP" -xf "$bundle"
  fi
  for o in $orgs; do
    code=$(curl $CURL -s -o /dev/null -w '%{http_code}' -u "$user:$token" -X POST \
      "$gitea/api/v1/orgs" -H 'Content-Type: application/json' -d "{\"username\":\"$o\"}")
    [ "$code" != 000 ] || die "连不上 $gitea（证书被拒就加 --insecure，或把 CA 装进系统信任区）"
    case "$code" in
      201)      say "建组织 $o ✓" ;;
      422|409)  say "组织 $o 已存在，跳过" ;;
      401|403)  die "建组织 $o 被拒（HTTP $code）——import 需要管理员 token" ;;
      *)        die "建组织 $o 失败：HTTP $code" ;;
    esac
  done

  for spec in "${ACTIONS[@]}"; do
    selected "$spec" || continue
    org="${spec%%/*}"; repo="${spec##*/}"; dir="$TMP/$(echo "$spec" | tr / _).git"
    say "== $spec"
    code=$(curl $CURL -s -o /dev/null -w '%{http_code}' -u "$user:$token" -X POST \
      "$gitea/api/v1/orgs/$org/repos" -H 'Content-Type: application/json' \
      -d "{\"name\":\"$repo\",\"private\":false,\"auto_init\":false,\"description\":\"mirror of github.com/$spec (v* refs)\"}")
    case "$code" in
      201)      say "  建仓库 ✓" ;;
      409|422)  say "  仓库已存在，跳过创建" ;;
      401|403)  die "  建仓库被拒（HTTP $code）" ;;
      *)        say "  建仓库返回 HTTP $code（继续尝试 push）" ;;
    esac

    if [ -z "$bundle" ]; then
      dir="$TMP/$(echo "$spec" | tr / _).git"
      say "  本地克隆（需要能访问 github.com）"
      git_clone "$spec" "$dir" || die "  clone 失败：$spec"
    fi
    [ -d "$dir" ] || die "  bundle 里没有 $spec —— 重新 export 一次"

    # 幂等：目标仓库已有 v* ref 就跳过。已是 pull mirror 的仓库也走这条
    # （Gitea 拒绝对 mirror 推送，报 "mirror repository is read-only"）。
    if [ "$REPUSH" = false ] && [ "$(tags_of "$gitea" "$user" "$token" "$spec")" != 0 ]; then
      say "  已有 tags，跳过推送（要强制重推加 --repush）"
      continue
    fi

    local ok=1
    for i in 1 2 3; do
      git_push "$dir" "$gitea" "$spec" "$user" "$token" && { ok=0; break; }
      say "  第 $i 次 push 失败，重试"
    done
    [ "$ok" = 0 ] || die "  push 失败：$spec"
    say "  完成，目标实例上 tags=$(tags_of "$gitea" "$user" "$token" "$spec")"
  done

  say "搬运完成。目标实例上还要做两件事（见 docs/installation/air-gapped.zh-cn.md）："
  say "  1) GITEA__actions__DEFAULT_ACTIONS_URL=self"
  say "  2) 收口：GITEA__service__DISABLE_REGISTRATION=true、GITEA__admin__DISABLE_REGULAR_ORG_CREATION=true"
}

tags_of() {  # $1=gitea $2=user $3=token $4=owner/repo
  curl $CURL -s -u "$2:$3" "$1/api/v1/repos/$4/tags?limit=100" | grep -o '"name"' | wc -l | tr -d ' '
}

[ $# -ge 1 ] || usage 1
case "$1" in
  export)    shift; cmd_export "$@" ;;
  import)    shift; cmd_import "$@" ;;
  -h|--help) usage 0 ;;
  *)         usage 1 ;;
esac
