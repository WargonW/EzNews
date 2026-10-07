#!/usr/bin/env bash
# EZNews 管理后台单文件端到端验证。
# 目的：在「真正编译出来的那一个二进制」上，把 /admin 后台涉及的全部管理端点跑一遍，
# 而不是只跑单元测试 —— 单测通过不代表装配对了（路由漏注册、中间件顺序错都在单测里看不见）。
set -uo pipefail

B=http://127.0.0.1:8080/api/v1
# ★ 必须显式清空代理环境变量。本机 shell 里设了 http_proxy=127.0.0.1:60301，
#   curl 会对 127.0.0.1 也走这个代理，于是所有请求都拿到 502 upstream connect failed，
#   而报错文本会让人以为是服务端挂了。同理不能用 --noproxy '*'：* 在 bash 里会被
#   glob 展开成当前目录文件名列表，反而把它变成一串无意义的主机名。
unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY ALL_PROXY all_proxy
CURL=(curl -s --max-time 10)
PASS=0; FAIL=0

ck() { # ck <描述> <实际> <期望>
  if [ "$2" = "$3" ]; then PASS=$((PASS+1)); printf '  \033[32m✓\033[0m %s\n' "$1";
  else FAIL=$((FAIL+1)); printf '  \033[31m✗\033[0m %s (实际=%s 期望=%s)\n' "$1" "$2" "$3"; fi
}

code() { ${CURL[@]} -o /dev/null -w '%{http_code}' "$@"; }
body() { ${CURL[@]} "$@"; }
jget() { python -c "import sys,json;d=json.load(sys.stdin);print(eval('d'+sys.argv[1]))" "$1" 2>/dev/null; }

echo "== A. 鉴权边界 =="
ck "未带令牌 401" "$(code $B/admin/overview)" 401
ck "乱令牌 401"   "$(code -H 'Authorization: Bearer nonsense' $B/admin/overview)" 401

echo "== B. 管理员登录 =="
LOGIN=$(body -X POST $B/auth/login -H 'Content-Type: application/json' \
  -d '{"username":"eztester","password":"Str0ng!Pass77"}')
AT=$(echo "$LOGIN" | jget "['data']['accessToken']")
[ -n "$AT" ] && ck "拿到 access token" yes yes || ck "拿到 access token" no yes
AH="Authorization: Bearer $AT"

echo "== C. 概览 =="
OV=$(body -H "$AH" $B/admin/overview)
ck "overview 200"   "$(code -H "$AH" $B/admin/overview)" 200
ck "有 users 字段"  "$(echo "$OV" | jget "['data']['users']" | grep -qE '^[0-9]+$' && echo yes)" yes
ck "有 admins 字段"  "$(echo "$OV" | jget "['data']['admins']" | grep -qE '^[0-9]+$' && echo yes)" yes
ck "generatedAt 是秒级" "$(echo "$OV" | python -c "import sys,json;v=json.load(sys.stdin)['data']['generatedAt'];print('yes' if 1_700_000_000<v<2_000_000_000 else 'no')")" yes

echo "== D. 用户列表与筛选 =="
UL=$(body -H "$AH" "$B/admin/users?limit=5")
ck "列表 200" "$(code -H "$AH" "$B/admin/users?limit=5")" 200
ck "items 是数组" "$(echo "$UL" | python -c "import sys,json;print('yes' if isinstance(json.load(sys.stdin)['data']['items'],list) else 'no')")" yes
ck "带 total" "$(echo "$UL" | jget "['data']['total']" | grep -qE '^[0-9]+$' && echo yes)" yes
ck "非法 role 返 400" "$(code -H "$AH" "$B/admin/users?role=root")" 400
ck "合法 role=admin 返 200" "$(code -H "$AH" "$B/admin/users?role=admin")" 200
ck "limit 越界 400（不静默截断）" "$(code -H "$AH" "$B/admin/users?limit=9999")" 400
ck "limit=0 400" "$(code -H "$AH" "$B/admin/users?limit=0")" 400
# ★ cursor 与 limit 刻意不同：cursor 只影响"从哪里开始"，非法就回第一页，无害；
#   limit 影响"返回多少行"，静默截断会让客户端误判。前者容错、后者报错是对的设计。
ck "游标非法回第一页 200" "$(code -H "$AH" "$B/admin/users?cursor=abc")" 200
ck "游标为负回第一页 200" "$(code -H "$AH" "$B/admin/users?cursor=-5")" 200
ck "游标缺省回落第一页" "$(code -H "$AH" "$B/admin/users")" 200

echo "== E. 能力位（服务端下发） =="
SELF=$(body -H "$AH" "$B/admin/users/$(echo "$OV" | jget "['data']['users']" >/dev/null; echo 0)" 2>/dev/null)
AID=$(echo "$LOGIN" | jget "['data']['user']['id']")
ME=$(body -H "$AH" $B/admin/users/$AID)
ck "自己 self=true"          "$(echo "$ME" | jget "['data']['self']")" True
ck "自己 canDelete=false"    "$(echo "$ME" | jget "['data']['canDelete']")" False
ck "自己 canDisable=false"   "$(echo "$ME" | jget "['data']['canDisable']")" False
ck "自己 canChangeRole=false" "$(echo "$ME" | jget "['data']['canChangeRole']")" False
ck "自己 canRevokeAll=false" "$(echo "$ME" | jget "['data']['canRevokeAll']")" False

echo "== F. 保护自己（应 409） =="
ck "降自己角色 409" "$(code -X PATCH -H "$AH" -H 'Content-Type: application/json' -d '{"role":"user"}' $B/admin/users/$AID/role)" 409
ck "停用自己 409"   "$(code -X PATCH -H "$AH" -H 'Content-Type: application/json' -d '{"disabled":true}' $B/admin/users/$AID/disabled)" 409
ck "删自己 409"     "$(code -X DELETE -H "$AH" $B/admin/users/$AID)" 409

echo "== G. 指针语义：disabled 缺字段必须 400 =="
BOB=$(body -H "$AH" "$B/admin/users?q=bob002" | jget "['data']['items'][0]['id']")
if [ "$BOB" != "None" ] && [ -n "$BOB" ]; then
  ck "缺 disabled 字段 400" "$(code -X PATCH -H "$AH" -H 'Content-Type: application/json' -d '{}' $B/admin/users/$BOB/disabled)" 400
  ck "非法 role 400"        "$(code -X PATCH -H "$AH" -H 'Content-Type: application/json' -d '{"role":"root"}' $B/admin/users/$BOB/role)" 400

  echo "== H. 停用普通用户（不得被最后管理员守卫误拦） =="
  ck "停用 bob 200" "$(code -X PATCH -H "$AH" -H 'Content-Type: application/json' -d '{"disabled":true}' $B/admin/users/$BOB/disabled)" 200
  ck "bob disabled=true" "$(body -H "$AH" $B/admin/users/$BOB | jget "['data']['disabled']")" True
  ck "重复停用幂等 200" "$(code -X PATCH -H "$AH" -H 'Content-Type: application/json' -d '{"disabled":true}' $B/admin/users/$BOB/disabled)" 200
  ck "启用 bob 200" "$(code -X PATCH -H "$AH" -H 'Content-Type: application/json' -d '{"disabled":false}' $B/admin/users/$BOB/disabled)" 200
  ck "bob disabled=false" "$(body -H "$AH" $B/admin/users/$BOB | jget "['data']['disabled']")" False
  ck "吊销 bob 会话 200" "$(code -X POST -H "$AH" $B/admin/users/$BOB/sessions/revoke-all)" 200
  ck "bob 现在 canDelete=true" "$(body -H "$AH" $B/admin/users/$BOB | jget "['data']['canDelete']")" True
else
  echo "  (跳过 G/H：库里没有 bob)"
fi

echo "== I. 404 语义 =="
ck "不存在用户 404" "$(code -H "$AH" $B/admin/users/999999)" 404
ck "改不存在用户 404" "$(code -X PATCH -H "$AH" -H 'Content-Type: application/json' -d '{"role":"user"}' $B/admin/users/999999/role)" 404
ck "删不存在用户 404" "$(code -X DELETE -H "$AH" $B/admin/users/999999)" 404

echo "== J. 代建账号 =="
U="e2e$RANDOM"
NEW=$(body -X POST -H "$AH" -H 'Content-Type: application/json' \
  -d "{\"username\":\"$U\",\"password\":\"Str0ng!Pass77\",\"role\":\"user\"}" $B/admin/users)
ck "代建 201" "$(code -X POST -H "$AH" -H 'Content-Type: application/json' -d "{\"username\":\"${U}b\",\"password\":\"Str0ng!Pass77\"}" $B/admin/users)" 201
ck "代建不带会话" "$(echo "$NEW" | jget "['data']['sessionCount']")" 0
ck "重名 409" "$(code -X POST -H "$AH" -H 'Content-Type: application/json' -d "{\"username\":\"$U\",\"password\":\"Str0ng!Pass77\"}" $B/admin/users)" 409
ck "弱密码 400" "$(code -X POST -H "$AH" -H 'Content-Type: application/json' -d '{"username":"weakpw1","password":"123"}' $B/admin/users)" 400

echo "== K. 内容统计 =="
CS=$(body -H "$AH" $B/admin/stats/content)
ck "内容统计 200" "$(code -H "$AH" $B/admin/stats/content)" 200
ck "topSources ≤10" "$(echo "$CS" | python -c "import sys,json;print('yes' if len(json.load(sys.stdin)['data']['topSources'])<=10 else 'no')")" yes
ck "orphanArticles 存在" "$(echo "$CS" | jget "['data']['orphanArticles']" | grep -qE '^[0-9]+$' && echo yes)" yes

echo "== L. 语音任务 =="
ck "任务列表 200" "$(code -H "$AH" $B/admin/audio/tasks)" 200
ck "status=failed 200" "$(code -H "$AH" "$B/admin/audio/tasks?status=failed")" 200
ck "status 非法 400" "$(code -H "$AH" "$B/admin/audio/tasks?status=bogus")" 400

echo "== M. 系统信息 =="
SI=$(body -H "$AH" $B/admin/system)
ck "系统信息 200" "$(code -H "$AH" $B/admin/system)" 200
ck "journalMode 存在" "$(echo "$SI" | jget "['data']['journalMode']" | grep -q 'wal' && echo yes)" yes
ck "dbBytes 是逻辑占用" "$(echo "$SI" | python -c "import sys,json;d=json.load(sys.stdin)['data'];print('yes' if d['dbBytes']==d['pageSize']*d['pageCount'] else 'no')")" yes

echo "== N. 降权/停用即时生效（权限来自库，不读 JWT） =="
# 目的：证明管理员手里的**未过期** access token 在降权后立刻失效。
# 做法：现场代建第二个管理员 secondadm → 用它登录拿 token → 确认能进后台 →
#      用 eztester 把它降成 user → 同一个 token 再请求必须 403（而不是等 2 小时过期）。
SU="secondadm$RANDOM"
code -X POST -H "$AH" -H 'Content-Type: application/json' \
  -d "{\"username\":\"$SU\",\"password\":\"Str0ng!Pass77\",\"role\":\"admin\"}" $B/admin/users >/dev/null
SL=$(body -X POST $B/auth/login -H 'Content-Type: application/json' \
  -d "{\"username\":\"$SU\",\"password\":\"Str0ng!Pass77\",\"clientId\":\"e2e-second\"}")
SAT=$(echo "$SL" | jget "['data']['accessToken']")
SID=$(echo "$SL" | jget "['data']['user']['id']")
if [ -n "$SAT" ] && [ "$SID" != "None" ]; then
  SAH="Authorization: Bearer $SAT"
  ck "第二管理员权限正常 200" "$(code -H "$SAH" $B/admin/overview)" 200
  ck "降级对方 200" \
    "$(code -X PATCH -H 'Content-Type: application/json' -H "$AH" -d '{"role":"user"}' $B/admin/users/$SID/role)" 200
  ck "对方旧 token 立刻 403（降权即时生效）" "$(code -H "$SAH" $B/admin/overview)" 403

  echo "== O. 停用即时生效（三条入口全拦） =="
  # ★ 刻意复用降权前那一步拿到的 token（SAT / SL），而不是重新登录：
  #   登录限流是 5/m（见 config.example.yaml 的 loginLimitPerUser），
  #   对同一账号反复登录会打到 429 —— 那是正确行为，但会让这里的断言失真。
  #   复用旧 token 恰好也更贴近真实场景：客户端手里本来就握着那张还没过期的。
  NAT="$SAT"
  NR=$(echo "$SL" | jget "['data']['refreshToken']")
  ck "停用对方 200" \
    "$(code -X PATCH -H 'Content-Type: application/json' -H "$AH" -d '{"disabled":true}' $B/admin/users/$SID/disabled)" 200
  ck "入口1 access 校验 403" "$(code -H "Authorization: Bearer $NAT" $B/admin/overview)" 403
  ck "入口2 重新登录 403" \
    "$(code -X POST $B/auth/login -H 'Content-Type: application/json' -d "{\"username\":\"$SU\",\"password\":\"Str0ng!Pass77\"}")" 403
  ck "入口3 refresh 403" \
    "$(code -X POST $B/auth/refresh -H 'Content-Type: application/json' -d "{\"refreshToken\":\"$NR\",\"clientId\":\"e2e-second\"}")" 403
  ck "停用后 canDisable=true（可被救回）" "$(body -H "$AH" $B/admin/users/$SID | jget "['data']['canDisable']")" True
  ck "启用恢复 200" \
    "$(code -X PATCH -H 'Content-Type: application/json' -H "$AH" -d '{"disabled":false}' $B/admin/users/$SID/disabled)" 200

  echo "== Q. 登录限流（5/m，安全属性） =="
  # 对同一账号连续登录，必然在若干次内拿到 429。这条不是在测"能不能登录"，
  # 而是在钉住"密码喷洒有速率上限"这个安全属性。
  RLU="rl$RANDOM"
  code -X POST -H "$AH" -H 'Content-Type: application/json' \
    -d "{\"username\":\"$RLU\",\"password\":\"Str0ng!Pass77\"}" $B/admin/users >/dev/null
  HIT429=no
  for i in 1 2 3 4 5 6 7 8; do
    C=$(code -X POST $B/auth/login -H 'Content-Type: application/json' -d "{\"username\":\"$RLU\",\"password\":\"wrong-password\"}")
    if [ "$C" = "429" ]; then HIT429=yes; break; fi
  done
  ck "反复尝试登录会触发 429" "$HIT429" yes

  echo "== P. 最后一个可用管理员守卫 =="
  # eztester 此刻是唯一未停用管理员（secondadm 刚被降成 user）。
  ck "唯一管理员不能降自己 409" \
    "$(code -X PATCH -H 'Content-Type: application/json' -H "$AH" -d '{"role":"user"}' $B/admin/users/$AID/role)" 409
  ck "唯一管理员不能停自己 409" \
    "$(code -X PATCH -H 'Content-Type: application/json' -H "$AH" -d '{"disabled":true}' $B/admin/users/$AID/disabled)" 409
  ck "唯一管理员不能删自己 409" "$(code -X DELETE -H "$AH" $B/admin/users/$AID)" 409
  # 但仍能正常管理普通用户 —— 这正是上一轮真 bug 的回归点：
  # 守卫若把"目标是不是管理员"的判断丢给调用方，这里会被误判成 409。
  ck "守卫不误伤普通用户（降级 bob002 为 user 应 200）" \
    "$(code -X PATCH -H 'Content-Type: application/json' -H "$AH" -d '{"role":"user"}' $B/admin/users/$BOB/role)" 200
else
  echo "  (跳过 N/O/P：第二管理员创建或登录失败)"
fi

echo
printf '结果: \033[32m%d 通过\033[0m / \033[31m%d 失败\033[0m\n' "$PASS" "$FAIL"

[ "$FAIL" -eq 0 ]
