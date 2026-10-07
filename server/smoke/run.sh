#!/usr/bin/env bash
# 一体化：启动服务端 → 等就绪 → 跑冒烟 → 关闭服务端
#
# ⚠️ 本文件必须存为 LF 行尾。CRLF 下 bash 会把行尾 \r 并进命令，
#    表现为 `line NN: $'\xxx': command not found` 这类指向纯 ASCII 行的
#    莫名报错，极难定位。smoke.sh 同理。
set -uo pipefail
cd "$(dirname "$0")/.."
ROOT="$(pwd)"
SMOKE="$ROOT/smoke"
BIN="${SMOKE_BIN:-/d/dev/softwares/eznews-data/bin/eznews.exe}"
PORT="${SMOKE_PORT:-18080}"

# ---- 互斥锁：禁止并发冒烟 ----
# 端口守卫只能挡住"进程残留"，挡不住"同时跑了两个 run.sh"：
# 第二个实例会清掉第一个正在用的 db、并与服务端抢端口，两边一起写
# result.txt / server.log，结果是三份 TOTAL 混在一份报告里，
# 且 `port_busy: command not found` 之类的报错满天飞，完全无法归因。
# 这里用原子 mkdir 做锁（比 flock 兼容性好，且跨 git-bash/PowerShell 通用）。
LOCK="$SMOKE/.smoke.lock"
if ! mkdir "$LOCK" 2>/dev/null; then
  if [ -f "$LOCK/pid" ]; then
    echo "FATAL: 已有冒烟实例在运行（pid $(cat "$LOCK/pid")）。"
    echo "若确认它已死，删除 $LOCK 后重试。"
  else
    echo "FATAL: 锁目录 $LOCK 已存在但无 pid，可能来自上次异常中断。"
    echo "确认无冒烟进程运行后，删除该目录再重试。"
  fi
  exit 1
fi
echo $$ > "$LOCK/pid"
# 退出时（含 Ctrl-C / set -e 早退）都清锁，否则一次失败就把冒烟永久锁死。
trap 'rm -rf "$LOCK" 2>/dev/null' EXIT INT TERM

# 端口必须每次独占：上一次遗留的进程会让新服务 bind 失败退出，
# 而测试会打到那个"状态不对"的旧进程上，产生一堆莫名其妙的连接失败。
#
# 两个坑（都踩过）：
#  1. 不能写成 "127.0.0.1:$PORT .*LISTENING" —— Windows netstat 的「本地地址」列是
#     固定宽度右对齐补空格（127.0.0.1:18080 后面跟一串 padding 才到外地址列），
#     要求 LISTENING 前恰好一个空格会匹配失败，守卫静默失效。
#  2. 不能只匹配 "127.0.0.1:$PORT[[:space:]]" —— netstat 的**外地址列**在客户端连接
#     行里也含 "127.0.0.1:18080"，上一轮跑完留下成百上千条 TIME_WAIT，
#     会被误判成"端口被占用"而直接拒绝开跑。
# 正确做法：只认 LISTENING 行，并且该行必须是本地地址列（行首）。
port_busy() {
  netstat -ano 2>/dev/null | grep -E "^\s+TCP\s+127\.0\.0\.1:$PORT[[:space:]]" | grep -q "LISTENING"
}

if command -v netstat >/dev/null 2>&1; then
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    port_busy || break
    echo "端口 $PORT 仍被占用，等待释放..."
    sleep 1
  done
  if port_busy; then
    echo "FATAL: 端口 $PORT 仍被 LISTEN 占用，无法运行冒烟测试。"
    echo "请手动结束占用进程后重试（Windows: taskkill //F //PID <pid>）。"
    netstat -ano | grep -E "^\s+TCP\s+127\.0\.0\.1:$PORT[[:space:]]" | grep "LISTENING"
    exit 1
  fi
fi

if [ ! -x "$BIN" ]; then
  echo "找不到服务端二进制: $BIN"
  echo "请先构建: go build -o $BIN ./cmd/eznews"
  exit 1
fi
echo "binary: $BIN"

rm -f "$SMOKE/eznews-smoke.db" "$SMOKE/eznews-smoke.db-wal" "$SMOKE/eznews-smoke.db-shm" \
      "$SMOKE/jwt_secret" "$SMOKE/smoke-cookies.txt" 2>/dev/null
rm -rf "$SMOKE/audio" 2>/dev/null

# smoke.yaml 里的路径是相对它的位置写的（./smoke/xxx 是相对 server/ 的），
# 所以必须以 server/ 为工作目录启动，否则会在别处生成一份散落的数据目录。
#
# ★ 必须用 EZNEWS_SERVER_ADDR 把端口传给服务端，不能只改 $PORT。
#   $PORT 只影响本脚本的端口守卫与健康检查地址；服务端读的是 smoke.yaml 里的
#   `server.addr`（写死 127.0.0.1:18080）。只设 SMOKE_PORT 而不传这个环境变量时，
#   服务端仍监听 18080，而脚本在 $PORT 上探健康检查 → 40 次轮询全失败 →
#   "SERVER FAILED TO BECOME READY" → 脚本退出，留下一个占着 18080 的孤儿 exe。
#   后果不止是这次失败：那个孤儿会让下一次冒烟 bind 失败，
#   而下一个人的 stop_server 会去杀它、却杀成另一个无辜进程（PID 三方对不上，见下）。
#   config.load.go:163 已支持 EZNEWS_SERVER_ADDR，优先级 YAML < env。
cd "$ROOT"
EZNEWS_SERVER_ADDR="127.0.0.1:$PORT" "$BIN" -config "$SMOKE/smoke.yaml" > "$SMOKE/server.log" 2>&1 &
SRV=$!
# ★ 实测结论（已用最小复现验证，勿再猜）：
#   git-bash 给 Windows 原生 exe 分配的是**自己命名空间里的 PID**，不是任务管理器里的那个。
#   实测：$! = 37026，而 netstat 显示真正监听的是 28252，tasklist 查 37026「没有匹配」。
#   所以 `taskkill //PID $SRV` 是**无效的**（这就是「bash 杀不掉原生 exe」的深层原因），
#   而 netstat 拿到的才是任务管理器视角的真实 PID。
#
# ★★ 但正因为它真实，绝不能在收尾时无条件去杀它 —— 那会杀掉**别人的**服务端。
#   本脚本只在「端口上的监听者是自己这个实例」时才动手：判据是启动后短暂等待，
#   监听者 PID 出现且在随后若干秒内保持不变。拿不到就交还给使用者，绝不乱杀。
echo "server pid=$SRV (git-bash 命名空间，非任务管理器 PID)"

# 轮询等待监听者出现，并记录它。绑定失败时新进程会立刻退出，此时端口上若无监听者，
# 说明是端口被别人占着—— 那属于并发冲突，应报FATAL 而不是抢着清理。
REAL_PID=""
for _ in 1 2 3 4 5 6 7 8 9 10; do
  CAND=$(netstat -ano 2>/dev/null \
    | grep -E "^\s+TCP\s+127\.0\.0\.1:$PORT[[:space:]]" | grep LISTENING \
    | awk '{print $5}' | head -1)
  if [ -n "$CAND" ]; then
    if [ "$CAND" = "${REAL_PID:-}" ]; then break; fi
    REAL_PID="$CAND"
  fi
  sleep 0.3
done
if [ -n "$REAL_PID" ]; then
  echo "server listening pid=$REAL_PID (任务管理器 PID，stop_server 只会杀它)"
else
  echo "WARN: 未观察到监听者 PID，可能端口 $PORT 被他人占用。stop_server 将不做清理。" >&2
fi

ok=0
for i in $(seq 1 40); do
  if curl -sS --noproxy '*' -o /dev/null "http://127.0.0.1:$PORT/healthz" 2>/dev/null; then ok=1; break; fi
  #进程已死就别再等了。
  # ★ 判据用「REAL_PID 是否还活着」而不是 `kill -0 $SRV`：
  #   $SRV 在 git-bash 的命名空间里，对原生 exe 的存活判断不可靠
  #   （exe 崩了它仍可能返回真，于是白等 20 秒才报 FAILED TO BECOME READY）。
  if [ -n "${REAL_PID:-}" ] && ! tasklist //FI "PID eq $REAL_PID" //FO CSV //NH 2>/dev/null | grep -q "$REAL_PID"; then
    echo "SERVER DIED DURING STARTUP"; cat "$SMOKE/server.log"; exit 1
  fi
  sleep 0.5
done
if [ "$ok" -ne 1 ]; then
  echo "SERVER FAILED TO BECOME READY"; cat "$SMOKE/server.log"
  # 启动失败也要清理，但**只清 REAL_PID**（确认是本实例启动的监听者）。
  # 绝不按端口盲杀：那会杀掉并发的另一个冒烟实例/ 开发服务器（本轮踩过）。
  if [ -n "${REAL_PID:-}" ]; then
    taskkill //F //PID "$REAL_PID" >/dev/null 2>&1
  else
    echo "无 REAL_PID，未执行任何清理（避免误杀他人进程）。" >&2
  fi
  exit 1
fi
echo "server ready"
echo

CK_="$SMOKE/smoke-cookies.txt"
export SMOKE_COOKIE_JAR="$CK_"
bash "$SMOKE/smoke.sh" "http://127.0.0.1:$PORT"
RC=$?

echo
echo "=== 服务端错误/警告日志 ==="
grep -Ei '"level":"(ERROR|WARN)"' "$SMOKE/server.log" | head -40
echo "=== 5xx 响应 ==="
grep -E '"status":5[0-9][0-9]' "$SMOKE/server.log" | head -20
echo "=== panic 检查 ==="
grep -ci panic "$SMOKE/server.log"

# 停服务：bash 内建 kill 对 Windows 原生 .exe 无效（杀不掉，进程会活到下次跑，
# 然后因为 bind 失败退出、测试全打到旧进程上）。必须走 taskkill //F //PID，
# 并确认端口真的释放了再返回，否则下一轮又会踩同一个坑。
#
# ★ 为什么必须连 REAL_PID 一起杀（血泪教训，本轮踩过）：
#   锁里记的是 bash 的 `$$`，而监听端口的是 exe 的 PID，两者**永远不是同一个值**。
#   于是会出现「锁显示持有者已死 / 端口仍在听 / 日志仍在写」三者同时成立。
#   本轮三个人各杀错一次：run.sh 以为在杀自己的 exe，实际杀的是上一轮遗留的孤儿；
#   另一个人以为在杀孤儿，实际杀的是别人正在跑的实例。链条根因就是这里只认 $SRV。
#   所以两个 PID 都杀，且都不做"它是不是我的"假设 —— 只看端口是否释放。
stop_server() {
  local pid="$1"
  # 端口守卫的互斥锁（mkdir 原子锁）已经保证同一时刻只有一个冒烟实例。
  # ★ 这里只杀 REAL_PID（netstat 观测到的、确认是自己启动的那个监听者）。
  #   绝不能杀 $SRV —— 实测 $! 在 git-bash 里是另一套命名空间的 PID，
  #   taskkill 它要么无效、要么（更糟）命中无关进程。
  if [ -z "${REAL_PID:-}" ]; then
    echo "stop_server: 无 REAL_PID，跳过清理（端口 $PORT 上若有监听者，不是本实例启动的）"
    return 0
  fi
  if command -v taskkill >/dev/null 2>&1; then
    taskkill //F //PID "$REAL_PID" >/dev/null 2>&1
  fi
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    if command -v netstat >/dev/null 2>&1; then
      # 复用同一个判定：只认 LISTENING，TIME_WAIT 不算占用。
      port_busy || { echo "server stopped"; return 0; }
    else
      echo "server stopped"
      return 0
    fi
    sleep 1
  done
  echo "WARNING: 端口 $PORT 释放失败，残留进程会污染下一次冒烟。" >&2
  netstat -ano | grep -E "^\s+TCP\s+127\.0\.0\.1:$PORT[[:space:]]" | grep "LISTENING" >&2
  return 1
}

stop_server "$SRV"
exit $RC
