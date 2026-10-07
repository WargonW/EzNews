#!/usr/bin/env bash
# EZNews 单文件构建入口
#
# 目标：产出一个**自带全套页面**的可执行文件 —— 服务端 API、Web 端阅读界面、
# /admin 管理后台全部被打进同一个二进制。部署就是运行它：
#
#     bash scripts/build.sh all
#     ./bin/eznews -config ./config.yaml
#
# 为什么是这个脚本而不是 Makefile 承担主要逻辑：
# 开发环境（Windows Git Bash）里没有 make，Makefile 无法执行；
# 而把构建逻辑同时写在 Makefile 和别处会让两者逐渐漂移。
# 所以实现只放在这里 —— Makefile 只是转发到本脚本的薄壳。
#
# 用法：
#   bash scripts/build.sh web        仅构建 Web 端产物
#   bash scripts/build.sh embed      把 Web 产物覆盖到 go:embed 读取的目录
#   bash scripts/build.sh server     仅编译服务端（不依赖前端产物）
#   bash scripts/build.sh all        完整单文件构建（发布用）
#   bash scripts/build.sh clean-ui   把 embed 目录还原成占位版本

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SERVER_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
WEB_DIR="$(cd "$SERVER_DIR/../web" && pwd)"
EMBED_DIST="$SERVER_DIR/internal/webui/dist"
PLACEHOLDER="$SERVER_DIR/internal/webui/.placeholder.html"
BINARY="eznews"
OUTDIR="$SERVER_DIR/bin"

# 这些是 go 的模块缓存位置。本机要求全部落在 D 盘，不能污染 C 盘。
export GOCACHE="${GOCACHE:-/d/dev/softwares/go-data/cache}"
export GOPATH="${GOPATH:-/d/dev/softwares/go-data/gopath}"
export GOMODCACHE="${GOMODCACHE:-/d/dev/softwares/go-data/gopath/pkg/mod}"
export GOFLAGS="${GOFLAGS:--mod=mod}"
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
export CGO_ENABLED=0

log()  { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
ok()   { printf '    ✓ %s\n' "$*"; }
die()  { printf '\n\033[31m✗ %s\033[0m\n' "$*" >&2; exit 1; }

# find_go 定位 go 可执行文件。
# 本机 go 装在 D:\dev\softwares\go，未必在 PATH 里。
find_go() {
  if command -v go >/dev/null 2>&1; then echo "go"; return 0; fi
  for cand in /d/dev/softwares/go/bin/go /d/dev/softwares/go/bin/go.exe; do
    [ -x "$cand" ] && { echo "$cand"; return 0; }
  done
  die "找不到 go。请安装 Go 或把 go 加入 PATH。"
}

# find_pnpm 定位 pnpm，并校验 registry 走国内镜像（本机强制要求）。
find_pnpm() {
  if command -v pnpm >/dev/null 2>&1; then echo "pnpm"; return 0; fi
  if [ -x /d/dev/softwares/nodejs-data/npm-global/pnpm ]; then
    echo /d/dev/softwares/nodejs-data/npm-global/pnpm; return 0
  fi
  die "找不到 pnpm。前端构建需要它（Web 端使用 pnpm workspace）。"
}

build_web() {
  log "构建 Web 端产物（含管理后台）"
  local pnpm_bin; pnpm_bin="$(find_pnpm)"
  [ -d "$WEB_DIR" ] || die "找不到 Web 端目录：$WEB_DIR"
  (cd "$WEB_DIR" && "$pnpm_bin" install --frozen-lockfile && "$pnpm_bin" run build) \
    || die "Web 端构建失败"
  [ -f "$WEB_DIR/dist/index.html" ] || die "构建完成但产物里没有 dist/index.html"
  ok "Web 端产物就绪：$WEB_DIR/dist"
}

embed_web() {
  log "把 Web 产物覆盖到 go:embed 目录"
  [ -f "$WEB_DIR/dist/index.html" ] || { die "没有前端产物，请先执行: bash $0 web"; }
  mkdir -p "$EMBED_DIST"
  # 覆盖而非「先清空再拷」：清空目录的瞬间若正好有别的读取方会看到不完整视图，
  # 且 dist/index.html 是编译必需文件（embed 空目录会直接失败）。
  cp -R "$WEB_DIR/dist/." "$EMBED_DIST/"
  ok "已覆盖：$EMBED_DIST"
}

build_server() {
  log "编译服务端二进制"
  local go_bin; go_bin="$(find_go)"
  mkdir -p "$OUTDIR"
  (cd "$SERVER_DIR" && "$go_bin" build -trimpath -ldflags "-s -w" -o "$OUTDIR/$BINARY" ./cmd/eznews) \
    || die "编译失败"
  ok "产出 $OUTDIR/$BINARY"
}

clean_ui() {
  log "还原 embed 目录为占位版本"
  mkdir -p "$EMBED_DIST"
  # 不依赖 git：本工程可能被归档分发而非 git 克隆，依赖 git 会让清理直接失效。
  find "$EMBED_DIST" -mindepth 1 ! -name '.placeholder*' -delete
  [ -f "$PLACEHOLDER" ] || die "缺少占位文件：$PLACEHOLDER"
  cp "$PLACEHOLDER" "$EMBED_DIST/index.html"
  ok "已还原（dist 下只剩占位 index.html）"
}

report() {
  local bin="$OUTDIR/$BINARY"
  log "构建完成"
  local size
  size="$(du -h "$bin" 2>/dev/null | cut -f1)"
  printf '    文件   %s\n' "$bin"
  printf '    体积   %s\n' "${size:-未知}"
  printf '    部署   %s -config ./config.yaml\n' "$bin"
}

case "${1:-all}" in
  web)      build_web ;;
  embed)    embed_web ;;
  server)   build_server ;;
  clean-ui) clean_ui ;;
  all)      build_web; embed_web; build_server; report ;;
  *)
    printf '用法: bash %s {all|web|embed|server|clean-ui}\n' "$0"
    exit 1
    ;;
esac
