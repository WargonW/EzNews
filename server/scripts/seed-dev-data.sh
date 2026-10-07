#!/usr/bin/env bash
# 联调种子数据：造出能验证「收藏页摘要字段」的真实数据。
#
# 目的：web 端收藏页在登录态下走 GET /me/favorites?withArticles=true，
# 依赖 StateItem 的 4 个摘要字段（title/summary/sourceName/publishedAt）。
# 空库时该页只能验证「空态」，验证不到摘要渲染 —— 所以这里必须造数据。
#
# ★ 本脚本可重复执行（幂等）：
#   1) 源：先查同名源，命中就复用其 key，不再新建。
#      generateSourceKey 对重名源加随机后缀，不复用会每次多出 3 个源。
#   2) 文章：ingest 按 contentHash 去重，第二次跑是 skipped(UNCHANGED)，不会翻倍。
#   3) 收藏/已读：按 articleId upsert，重复写不产生重复行。
#
# 造完后请用 dev 账号登录前端查看：
#   账号 devuser / 密码 DevPassw0rd!2026
#
# ★ 必须 LF 行尾。CRLF 会让 bash 把 \r 并进命令，表现为
#   "$'xxx\r': command not found" 之类的诡异报错。
set -uo pipefail

BASE="${1:-http://127.0.0.1:8080}"
B="$BASE/api/v1"
J="Content-Type: application/json"
KEY="dev-key-local-only"
PY="C:/Users/51936/.workbuddy/binaries/python/versions/3.13.12/python.exe"
export NO_PROXY="127.0.0.1,localhost" no_proxy="127.0.0.1,localhost"

c()  { curl -sS --noproxy '*' "$@"; }
ex() { $PY -c "
import sys,json
try:
    d=json.load(sys.stdin)
    print($1)
except Exception as e:
    print('ERR:'+type(e).__name__)
" 2>/dev/null; }

if ! c -o /dev/null "$BASE/healthz" 2>/dev/null; then
  echo "FATAL: 服务未启动（$BASE/healthz 不通）。先启动服务端。"
  exit 1
fi

# ---- 源：先按名字查已有源，命中复用 key，否则新建 ----
# SourceInput 不接受 key（openapi 如此），key 一律由服务端按 name 生成。
#
# ★ 进度日志必须走 stderr：本函数用 stdout 传 key，若日志也进 stdout，
#   调用方的 $(...) 会把日志一起吞进 sourceKey，表现为
#   "新建源 联调科技 -> custom-csnr\ncustom-csnr" 这种脏值。
ensure_source() { # $1=name $2=url $3=category -> 打印 key
  local name="$1" url="$2" cat="$3" key code
  key=$(c "$B/sources" | $PY -c "
import sys,json
name=sys.argv[1]
try:
    items=json.load(sys.stdin)['data']['items']
except Exception:
    print(''); raise SystemExit
hit=[s for s in items if s.get('name')==name]
print(hit[0]['key'] if hit else '')
" "$name")
  if [ -n "$key" ]; then
    echo "  复用源 $name -> $key" >&2
  else
    local r; r=$(c -X POST "$B/sources" -H "$J" -d "{\"name\":\"$name\",\"url\":\"$url\",\"type\":\"rss\",\"category\":\"$cat\"}")
    code=$(echo "$r" | ex 'd.get("code","")')
    key=$(echo "$r" | ex 'd["data"]["key"]')
    if [ -z "$key" ] || [ "${key:0:4}" = "ERR:" ]; then
      echo "  建源失败 $name: $(echo "$r" | head -c 160)" >&2
      return 1
    fi
    echo "  新建源 $name -> $key ($code)" >&2
  fi
  printf '%s' "$key"
}

echo "--- 准备源 ---"
K_TECH=$(ensure_source "联调科技" "https://tech.example.com/rss" "tech")
K_FIN=$(ensure_source "联调财经" "https://fin.example.com/rss" "finance")
K_WORLD=$(ensure_source "联调国际" "https://world.example.com/rss" "world")
for v in "$K_TECH" "$K_FIN" "$K_WORLD"; do
  if [ -z "$v" ] || [ "${v:0:4}" = "ERR:" ]; then
    echo "FATAL: 源 key 获取失败，中止（否则文章会全部 SOURCE_NOT_FOUND）"
    exit 1
  fi
done

# ---- 时间戳必须动态生成 ----
# 服务端校验 publishedAt ∈ [now-365d, now+24h]，写死日期会全部 UNPROCESSABLE。
ts() { $PY -c "
import datetime as d,sys
print((d.datetime.now(d.timezone.utc)-d.timedelta(hours=int(sys.argv[1]))).strftime('%Y-%m-%dT%H:%M:%SZ'))
" "$1"; }
H1=$(ts 1); H2=$(ts 2); H3=$(ts 3); H6=$(ts 6); H12=$(ts 12); H20=$(ts 20); H30=$(ts 30)

# ---- 造 9 篇文章 ----
# 摘要字段要有真实长度差异：短的空串、长的上百字，
# 便于在页面上看出截断/换行/空态是否正常。
# verbose=all 才能拿到每条的 articleId —— 收藏要按真实 id 写，
# 硬编码 1..9 在重复执行或库里有旧数据时会指向别人的文章。
ing() { # $1=body -> 打印 "created updated skipped id1,id2,... [|失败:...]"
  c -X POST "$B/ingest/articles?verbose=all" -H "X-Api-Key: $KEY" -H "$J" -d "$1" | $PY -c "
import sys,json
raw=sys.stdin.read()
try:
    env=json.loads(raw)
except Exception:
    print('ERR:非JSON响应:'+raw[:160]); raise SystemExit
if env.get('code')!='OK':
    print('ERR:%s %s' % (env.get('code'), (env.get('message') or '')[:120])); raise SystemExit
d=env['data']
res=d.get('results') or []
ids=[str(r['articleId']) for r in res if r.get('status')!='failed' and r.get('articleId')]
fails=[('%s/%s' % (r.get('reason'), (r.get('message') or '')[:60])) for r in res if r.get('status')=='failed']
print('%d %d %d %s %s' % (d['created'], d['updated'], d['skipped'], ','.join(ids), '|失败:'+';'.join(fails) if fails else ''))
"
}

echo "--- 灌入文章 ---"
ALL_IDS=""
add() { # $1=body
  local r; r=$(ing "$1")
  local st=${r%% *}
  if [ "${st:0:4}" = "ERR:" ] || [ -z "$r" ]; then
    echo "  失败: $(echo "$1" | head -c 90)... -> ${r:-空响应}" >&2
    return 1
  fi
  local ids rest
  ids=$(echo "$r" | awk '{print $4}')
  rest=$(echo "$r" | cut -d' ' -f5-)
  if [ -z "$ids" ]; then
    echo "  未拿到 articleId: $rest" >&2
    return 1
  fi
  ALL_IDS="$ALL_IDS,$ids"
  echo "  c=$(echo "$r"|awk '{print $1}') u=$(echo "$r"|awk '{print $2}') s=$(echo "$r"|awk '{print $3}') id=$ids ${rest:+ $rest}" >&2
}

add "{\"sourceKey\":\"$K_TECH\",\"externalId\":\"tech-1\",\"url\":\"https://tech.example.com/1\",\"title\":\"国产大模型推理成本再降一个数量级\",\"summary\":\"新一代稀疏MoE 架构把单次推理成本压到原来的十分之一，某头部云厂商已宣布全线上线。\",\"publishedAt\":\"$H1\"}"
add "{\"sourceKey\":\"$K_TECH\",\"externalId\":\"tech-2\",\"url\":\"https://tech.example.com/2\",\"title\":\"折叠屏出货量同比增长三倍\",\"summary\":\"二季度全球折叠屏出货量同比增长 312%，厂商称价格下探是关键驱动。\",\"publishedAt\":\"$H3\"}"
add "{\"sourceKey\":\"$K_TECH\",\"externalId\":\"tech-3\",\"url\":\"https://tech.example.com/3\",\"title\":\"开源社区发布端侧推理框架新版本\",\"summary\":\"\",\"publishedAt\":\"$H6\"}"
add "{\"sourceKey\":\"$K_FIN\",\"externalId\":\"fin-1\",\"url\":\"https://fin.example.com/1\",\"title\":\"央行开展4000亿元MLF操作 中标利率持平\",\"summary\":\"本次MLF操作期限一年，中标利率与上月持平，市场流动性合理充裕。\",\"publishedAt\":\"$H2\"}"
add "{\"sourceKey\":\"$K_FIN\",\"externalId\":\"fin-2\",\"url\":\"https://fin.example.com/2\",\"title\":\"新能源车出海遇关税调查 行业称影响可控\",\"summary\":\"涉及金额约 18 亿美元，相关企业已启动多元化市场布局。\",\"publishedAt\":\"$H12\"}"
add "{\"sourceKey\":\"$K_FIN\",\"externalId\":\"fin-3\",\"url\":\"https://fin.example.com/3\",\"title\":\"半导体设备订单连续三月回升\",\"summary\":\"晶圆厂扩产带动设备需求，进口替代进程同步加快。\",\"publishedAt\":\"$H20\"}"
add "{\"sourceKey\":\"$K_WORLD\",\"externalId\":\"world-1\",\"url\":\"https://world.example.com/1\",\"title\":\"多国就清洁能源融资机制达成框架协议\",\"summary\":\"协议覆盖发展中国家转型融资，规模与分担机制仍待后续磋商。\",\"publishedAt\":\"$H6\"}"
add "{\"sourceKey\":\"$K_WORLD\",\"externalId\":\"world-2\",\"url\":\"https://world.example.com/2\",\"title\":\"全球航运价格指数回落至三年低位\",\"summary\":\"红海局势缓解后，主要航线运价回落。\",\"publishedAt\":\"$H30\"}"
add "{\"sourceKey\":\"$K_WORLD\",\"externalId\":\"world-3\",\"url\":\"https://world.example.com/3\",\"title\":\"国际空间站下一批实验项目确定\",\"summary\":\"包含材料科学与微重力生命科学方向。\",\"publishedAt\":\"$H6\"}"

ALL_IDS="${ALL_IDS#,}"
N_IDS=$(echo "$ALL_IDS" | tr ',' '\n' | grep -c . || true)
if [ "$N_IDS" -lt 9 ]; then
  echo "FATAL: 只拿到 $N_IDS 个文章 id（需要 9），中止（否则收藏会指向不存在的文章）"
  exit 1
fi

# ---- 建账号（已存在则复用）----
# 用户名字符集：字母/数字/下划线/短横线/点（auth_service.validateUsername）。
# 不含 '@' —— 用邮箱当用户名会直接 VALIDATION_FAILED。
U="devuser"; PW='DevPassw0rd!2026'; CID="dev-cli-local"
R=$(c -X POST "$B/auth/register" -H "$J" -d "{\"username\":\"$U\",\"password\":\"$PW\",\"clientId\":\"$CID\"}")
CODE=$(echo "$R" | ex 'd.get("code","")')
if [ "$CODE" != "OK" ]; then
  # USERNAME_TAKEN 是正常分支；其它码要打出来，否则登录失败时看不出真正原因。
  MSG=$(echo "$R" | ex 'd.get("message","")')
  echo "注册返回 $CODE ($MSG)，改为登录"
  R=$(c -X POST "$B/auth/login" -H "$J" -d "{\"username\":\"$U\",\"password\":\"$PW\",\"clientId\":\"$CID\"}")
fi
ACC=$(echo "$R" | ex 'd["data"]["accessToken"]')
if [ -z "$ACC" ] || [ "${ACC:0:4}" = "ERR:" ]; then
  echo "FATAL: 取 token 失败：$(echo "$R" | head -c 240)"
  exit 1
fi
AU="Authorization: Bearer $ACC"
echo "账号就绪: $U"

# ---- 收藏 5 篇（从真实 id 里挑，覆盖不同发布时间以体现"最新在前"的排序差异）----
pick() { echo "$ALL_IDS" | cut -d, -f"$1"; }
FAV_IDS="$(pick 1),$(pick 2),$(pick 4),$(pick 5),$(pick 7)"
echo "--- 写入收藏: $FAV_IDS ---"
for id in ${FAV_IDS//,/ }; do
  c -X POST -H "$AU" -H "$J" -d "{\"articleId\":$id,\"deleted\":false}" "$B/me/favorites" >/dev/null
done
# 其中 2 篇标记已读，验证 isRead 交互
for id in "$(pick 1),$(pick 2)"; do
  for one in ${id//,/ }; do
    c -X POST -H "$AU" -H "$J" -d "{\"articleId\":$one,\"deleted\":false}" "$B/me/reads" >/dev/null
  done
done

# ---- 验证收藏页依赖的接口确实带摘要字段 ----
echo ""
echo "=== 验证 GET /me/favorites?withArticles=true ==="
FAV=$(c -H "$AU" "$B/me/favorites?withArticles=true")
echo "$FAV" | $PY -c "
import sys,json
d=json.load(sys.stdin)
items=d['data']['items']
print('收藏条数:', len(items),' hasMore:', d['data'].get('hasMore'))
missing=0
for i in items:
    have=[k for k in ('title','summary','sourceName','publishedAt') if i.get(k)]
    print('  id=%s title=%r sourceName=%r publishedAt=%r 字段=%d/4' % (
        i['articleId'], (i.get('title') or '')[:22], i.get('sourceName'), i.get('publishedAt'), len(have)))
    if len(have)<4: missing+=1
print('摘要字段不全的条数:', missing, '（0 = 前端可正常渲染）')
"

echo ""
echo "=== 验证 withArticles=false 不带摘要（前端 LWW 合并走这条） ==="
c -H "$AU" "$B/me/favorites?withArticles=false" | $PY -c "
import sys,json
items=json.load(sys.stdin)['data']['items']
has=any('title' in i for i in items)
print('  是否含 title 字段:', has, '（应为 False）')
"

echo ""
echo "联调账号: $U / $PW"
echo "文章 id: $ALL_IDS"
echo "可访问: $BASE/api/v1/articles （前端 dev server 见 http://127.0.0.1:5173）"
