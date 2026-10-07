#!/usr/bin/env bash
# EZNews server end-to-end smoke test
# Usage: bash smoke/smoke.sh [base_url]
#
# All paths/field names verified against real source:
#   internal/httpapi/router.go    route table
#   internal/model/ingest.go      IngestReceipt{created,updated,skipped,failed,results[]}
#   internal/httpapi/dto_me.go    StateItem / SetStateRequest / BatchStateRequest / BatchStateResultDTO / PutPreferencesRequest
#
# NOTE: publishedAt must be within [now-365d, now+24h] (server rejects otherwise
# with UNPROCESSABLE). All timestamps here are generated dynamically.
set -uo pipefail

BASE="${1:-http://127.0.0.1:18080}"
B="$BASE/api/v1"
KEY="smoke-key-do-not-use-in-prod"
export NO_PROXY="127.0.0.1,localhost" no_proxy="127.0.0.1,localhost"

PASS=0; FAIL=0
J="Content-Type: application/json"
PY="python"

c()  { curl -sS --noproxy '*' "$@"; }
ck() {
  if [ "$2" = "$3" ]; then
    printf '  \033[32mPASS\033[0m %-52s %s\n' "$1" "$3"; PASS=$((PASS+1))
  else
    printf '  \033[31mFAIL\033[0m %-52s got=[%s] want=[%s]\n' "$1" "$3" "$2"; FAIL=$((FAIL+1))
  fi
}
sec() { printf '\n\033[1m== %s ==\033[0m\n' "$1"; }
ex() { $PY -c "
import sys,json
try:
    d=json.load(sys.stdin)
    print($1)
except Exception as e:
    print('ERR:'+type(e).__name__)
" 2>/dev/null; }
is4() { case "$1" in 4*) echo yes;; *) echo no;; esac; }
is2() { case "$1" in 2*) echo yes;; *) echo no;; esac; }
code() { c -o /dev/null -w '%{http_code}' "$@"; }

# 动态时间戳（服务端窗口：now-365d ~ now+24h）
T1=$($PY -c "import datetime as d;print((d.datetime.now(d.timezone.utc)-d.timedelta(days=2)).strftime('%Y-%m-%dT%H:%M:%SZ'))")
T2=$($PY -c "import datetime as d;print((d.datetime.now(d.timezone.utc)-d.timedelta(days=1)).strftime('%Y-%m-%dT%H:%M:%SZ'))")
T3=$($PY -c "import datetime as d;print(d.datetime.now(d.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ'))")
TZ8=$($PY -c "import datetime as d;print((d.datetime.now(d.timezone.utc)-d.timedelta(hours=3)).strftime('%Y-%m-%dT%H:%M:%S+08:00'))")
NOW_MS=$($PY -c "import time;print(int(time.time()*1000))")

U="smokeu$RANDOM"; PW='Passw0rd!smoke'; NPW='NewPassw0rd!smoke'
RN=$RANDOM; CID="cli-$RN"

# ==================== 0. 准备：显式建源（autoCreateSource=false 才能验证 SOURCE_NOT_FOUND）
sec "0. Setup: create source explicitly"
SRC_KEY="smoke$RN"
# 契约：SourceInput 无 key 字段，key 由服务端从 name 自动生成（generateSourceKey）。
CREATED=$(c -X POST "$B/sources" -H "$J" -d "{\"name\":\"Smoke$RN\",\"url\":\"https://smoke.example.com/rss\",\"type\":\"rss\",\"category\":\"tech\"}")
ck "POST /sources -> 201" 201 "$(code -X POST "$B/sources" -H "$J" -d "{\"name\":\"SmokeDup$RN\",\"url\":\"https://smoke.example.com/rss2\",\"type\":\"rss\",\"category\":\"tech\"}")"
SRC=$(echo "$CREATED" | ex 'd["data"]["id"]')
SRC_KEY=$(echo "$CREATED" | ex 'd["data"]["key"]')
ck "server auto-generates key" yes "$([ -n "$SRC_KEY" ] && echo yes || echo no)"
echo "     sourceId=$SRC key=$SRC_KEY"
# 把后续用例里的 sourceKey 换成真实存在的 key
A1="{\"sourceKey\":\"$SRC_KEY\",\"externalId\":\"e-1\",\"url\":\"https://smoke.example.com/1?utm_source=rss\",\"title\":\"Fold phone launched\",\"summary\":\"Hinge life up\",\"publishedAt\":\"$TZ8\"}"
A1B="{\"sourceKey\":\"$SRC_KEY\",\"externalId\":\"e-1\",\"url\":\"https://smoke.example.com/1?utm_source=feed\",\"title\":\"Fold phone launched\",\"summary\":\"Hinge life up\",\"publishedAt\":\"$TZ8\"}"
A2="{\"sourceKey\":\"$SRC_KEY\",\"externalId\":\"e-1\",\"url\":\"https://smoke.example.com/1\",\"title\":\"Fold phone launched (revised)\",\"summary\":\"Hinge life up\",\"publishedAt\":\"$TZ8\"}"

# ==================== 1. health
sec "1. Health"
ck "GET /healthz" 200 "$(code $BASE/healthz)"
ck "GET /readyz"  200 "$(code $BASE/readyz)"
ck "GET /metrics" 200 "$(code $BASE/metrics)"
ck "healthz has requestId" yes "$(c $BASE/healthz | ex '"yes" if "requestId" in d else "no"')"
ck "readyz db=ok" ok "$(c $BASE/readyz | ex 'd["data"]["db"]')"
ck "healthz reports ttsQueue" yes "$(c $BASE/healthz | ex '"yes" if "ttsQueue" in d["data"]["dependencies"] else "no"')"

# ==================== 2. ingest auth
sec "2. ingest auth (X-Api-Key, independent of accounts)"
IT="{\"sourceKey\":\"$SRC_KEY\",\"url\":\"https://smoke.example.com/nokey\",\"title\":\"x\",\"publishedAt\":\"$T1\"}"
ck "no key -> 401" 401 "$(code -X POST "$B/ingest/articles" -H "$J" -d "$IT")"
ck "wrong key -> 401" 401 "$(code -X POST "$B/ingest/articles" -H "X-Api-Key: wrong-key" -H "$J" -d "$IT")"
ck "Bearer cannot act as apiKey" 401 "$(code -X POST "$B/ingest/articles" -H "Authorization: Bearer x" -H "$J" -d "$IT")"
# 单条业务失败在 data.results 里返回（openapi: "已接收并处理（业务级失败通过 data.results 返回）"），
# HTTP 仍是 200；只有整批请求层失败（鉴权/JSON 体/verbose 参数）才 4xx。
ck "missing url -> results failed" VALIDATION_FAILED "$(c -X POST "$B/ingest/articles?verbose=all" -H "X-Api-Key: $KEY" -H "$J" -d "{\"sourceKey\":\"$SRC_KEY\",\"title\":\"n\",\"publishedAt\":\"$T1\"}" | ex 'd["data"]["results"][0]["reason"]')"
ck "missing title -> results failed" VALIDATION_FAILED "$(c -X POST "$B/ingest/articles?verbose=all" -H "X-Api-Key: $KEY" -H "$J" -d "{\"sourceKey\":\"$SRC_KEY\",\"url\":\"https://s.example.com/nt\",\"publishedAt\":\"$T1\"}" | ex 'd["data"]["results"][0]["reason"]')"
ck "bad publishedAt -> results failed" VALIDATION_FAILED "$(c -X POST "$B/ingest/articles?verbose=all" -H "X-Api-Key: $KEY" -H "$J" -d '{"sourceKey":"smoke","url":"https://s.example.com/bd","title":"b","publishedAt":"not-a-date"}' | ex 'd["data"]["results"][0]["reason"]')"
ck "field error names the field" "items[0].url"  "$(c -X POST "$B/ingest/articles?verbose=all" -H "X-Api-Key: $KEY" -H "$J" -d "{\"sourceKey\":\"$SRC_KEY\",\"title\":\"n\",\"publishedAt\":\"$T1\"}" | ex 'd["data"]["results"][0]["field"]')"
ck "publishedAt too old -> UNPROCESSABLE" UNPROCESSABLE "$(c -X POST "$B/ingest/articles?verbose=all" -H "X-Api-Key: $KEY" -H "$J" -d '{"sourceKey":"smoke","url":"https://s.example.com/old","title":"old","publishedAt":"2020-01-01T00:00:00Z"}' | ex 'd["data"]["results"][0]["reason"]')"
ck "bad verbose -> 4xx" yes "$(is4 "$(code -X POST "$B/ingest/articles?verbose=bogus" -H "X-Api-Key: $KEY" -H "$J" -d "$IT")")"
ck "malformed JSON -> 4xx" yes "$(is4 "$(code -X POST "$B/ingest/articles" -H "X-Api-Key: $KEY" -H "$J" -d '{not json')")"
ck "non-http url -> VALIDATION_FAILED" VALIDATION_FAILED "$(c -X POST "$B/ingest/articles?verbose=all" -H "X-Api-Key: $KEY" -H "$J" -d "{\"sourceKey\":\"$SRC_KEY\",\"url\":\"javascript:alert(1)\",\"title\":\"x\",\"publishedAt\":\"$T1\"}" | ex 'd["data"]["results"][0]["reason"]')"

# ==================== 3. ingest tri-state
sec "3. ingest contentHash tri-state"
ING() { c -X POST "$B/ingest/articles?verbose=all" -H "X-Api-Key: $KEY" -H "$J" -d "$1"; }

R=$(ING "$A1")
ck "first write created=1" 1 "$(echo "$R" | ex 'd["data"]["created"]')"
ck "receipt status=created" created "$(echo "$R" | ex 'd["data"]["results"][0]["status"]')"
ART_ID=$(echo "$R" | ex 'd["data"]["results"][0]["articleId"]')
echo "     articleId=$ART_ID"
R=$(ING "$A1B")
ck "identical resubmit skipped=1" 1 "$(echo "$R" | ex 'd["data"]["skipped"]')"
ck "skip reason=UNCHANGED" UNCHANGED "$(echo "$R" | ex 'd["data"]["results"][0]["reason"]')"
R=$(ING "$A2")
ck "title change updated=1" 1 "$(echo "$R" | ex 'd["data"]["updated"]')"
ck "updated keeps same articleId" "$ART_ID" "$(echo "$R" | ex 'd["data"]["results"][0]["articleId"]')"
R=$(c -X POST "$B/ingest/articles?verbose=all" -H "X-Api-Key: $KEY" -H "$J" -d "{\"sourceKey\":\"nosuch-$RN\",\"url\":\"https://s.example.com/nf\",\"title\":\"x\",\"publishedAt\":\"$T1\"}")
ck "unknown sourceKey -> SOURCE_NOT_FOUND" SOURCE_NOT_FOUND "$(echo "$R" | ex 'd["data"]["results"][0]["reason"]')"

# ==================== 3b. category 缺省继承
sec "3b. category inherit from source"
# ★ 这段锁的是「category 参与 contentHash」+「缺省继承源分类」两个机制的合力。
#   回归症状（2026-10-06 修的真 bug）：采集器第二轮不传 category 时，
#   文章分类从 finance 被静默降级为 other，且每轮都刷 updated_at 污染增量游标。
#   $SRC_KEY 这个源建源时 category=tech（第 58 行），所以继承值应为 tech。
CAT_EXPL='{"sourceKey":"'$SRC_KEY'","externalId":"cat-x'$RN'","url":"https://smoke.example.com/catx'$RN'","title":"Category Inherit Probe","publishedAt":"'$T1'","category":"tech"}'
CAT_OMIT='{"sourceKey":"'$SRC_KEY'","externalId":"cat-x'$RN'","url":"https://smoke.example.com/catx'$RN'","title":"Category Inherit Probe","publishedAt":"'$T1'"}'
R=$(ING "$CAT_EXPL")
ck "explicit category created=1" 1 "$(echo "$R" | ex 'd["data"]["created"]')"
CAT_ID=$(echo "$R" | ex 'd["data"]["results"][0]["articleId"]')
R=$(ING "$CAT_OMIT")
# ★ 核心断言：必须 skipped。若这里变 updated，说明继承值没参与指纹计算（算在了解析源之前）。
ck "omit category after explicit -> skipped=1 (not updated)" 1 "$(echo "$R" | ex 'd["data"]["skipped"]')"
ck "skip reason=UNCHANGED" UNCHANGED "$(echo "$R" | ex 'd["data"]["results"][0]["reason"]')"
CAT_CAT=$(c "$B/articles/$CAT_ID" | ex 'd["data"]["category"]')
ck "category preserved as source's tech" tech "$CAT_CAT"
# 显式 other 不得被继承值覆盖（同一轮内）
R=$(ING '{"sourceKey":"'$SRC_KEY'","externalId":"cat-y'$RN'","url":"https://smoke.example.com/caty'$RN'","title":"Explicit Other Probe","publishedAt":"'$T1'","category":"other"}')
CAT_Y=$(echo "$R" | ex 'd["data"]["results"][0]["articleId"]')
ck "explicit other wins over source tech" other "$(c "$B/articles/$CAT_Y" | ex 'd["data"]["category"]')"
# 纯缺省条目应直接落源分类，且重复投必须幂等
R=$(ING '{"sourceKey":"'$SRC_KEY'","externalId":"cat-z'$RN'","url":"https://smoke.example.com/catz'$RN'","title":"Pure Omit Probe","publishedAt":"'$T1'"}')
CAT_Z=$(echo "$R" | ex 'd["data"]["results"][0]["articleId"]')
ck "pure omit inherits source category" tech "$(c "$B/articles/$CAT_Z" | ex 'd["data"]["category"]')"
R=$(ING '{"sourceKey":"'$SRC_KEY'","externalId":"cat-z'$RN'","url":"https://smoke.example.com/catz'$RN'","title":"Pure Omit Probe","publishedAt":"'$T1'"}')
ck "pure omit resubmit skipped=1" 1 "$(echo "$R" | ex 'd["data"]["skipped"]')"

# ==================== 4. batch ingest
sec "4. batch ingest (per-item receipts)"
# ⚠️ 这里刻意用一条全新的条目而不是复用 $A1：
#    $A1 的标题是初版，而第 3 段末尾已用 $A2 把它改成 "(revised)"。
#    批量里再投一次 $A1 会把标题改回初版，导致后面第 5 段
#    "title is revised version" 断言失败——一个纯粹的测试数据自相矛盾。
BATCH=$(printf '{"items":[%s,%s,%s]}' \
 "{\"sourceKey\":\"$SRC_KEY\",\"externalId\":\"b-1$RN\",\"url\":\"https://smoke.example.com/b1$RN\",\"title\":\"Batch One\",\"publishedAt\":\"$T2\"}" \
 "{\"sourceKey\":\"$SRC_KEY\",\"externalId\":\"e-2\",\"url\":\"https://smoke.example.com/2\",\"title\":\"Second\",\"publishedAt\":\"$T2\"}" \
 "{\"sourceKey\":\"$SRC_KEY\",\"externalId\":\"e-3\",\"url\":\"https://smoke.example.com/3\",\"title\":\"Third\",\"publishedAt\":\"$T2\"}")
R=$(c -X POST "$B/ingest/articles/batch?verbose=all" -H "X-Api-Key: $KEY" -H "$J" -d "$BATCH")
ck "batch received=3" 3 "$(echo "$R" | ex 'd["data"]["received"]')"
ck "batch failed=0" 0 "$(echo "$R" | ex 'd["data"]["failed"]')"
ck "batch results len=3" 3 "$(echo "$R" | ex 'len(d["data"]["results"])')"
ck "empty items -> 400" 400 "$(code -X POST "$B/ingest/articles/batch" -H "X-Api-Key: $KEY" -H "$J" -d '{"items":[]}')"
BATCH2=$(printf '{"items":[%s,%s]}' \
 "{\"sourceKey\":\"$SRC_KEY\",\"externalId\":\"e-ok$RN\",\"url\":\"https://s.example.com/ok$RN\",\"title\":\"Good\",\"publishedAt\":\"$T2\"}" \
 "{\"sourceKey\":\"$SRC_KEY\",\"url\":\"https://s.example.com/bad$RN\",\"title\":\"Bad-no-date\"}")
# ⚠️ BATCH2 只能提交一次：先跑一次就会把 good item 建出来，
# 再跑第二遍它是 skipped（UNCHANGED）而非 created，created 断言必挂。
# 要同时验证「HTTP 层不失败」和「明细计数」，就靠 verbose=all 这一次的
# 响应体里既有 HTTP 200 又有 per-item receipt，不需要额外再打一遍。
R=$(c -X POST "$B/ingest/articles/batch?verbose=all" -H "X-Api-Key: $KEY" -H "$J" -d "$BATCH2")
ck "one bad item does not fail batch" OK "$(echo "$R" | ex 'd["code"]')"
ck "good item created=1" 1 "$(echo "$R" | ex 'd["data"]["created"]')"
ck "bad item failed=1" 1 "$(echo "$R" | ex 'd["data"]["failed"]')"
# oversized batch -> 413
BIG=$(printf '{"items":[%s]}' "$(for i in $(seq 1 205); do printf '{"sourceKey":"smoke","externalId":"big%s","url":"https://s.example.com/b%s","title":"t%s","publishedAt":"%s"},' "$i" "$i" "$i" "$T2"; done | sed 's/,$//')")
ck "205 items -> 413" 413 "$(code -X POST "$B/ingest/articles/batch" -H "X-Api-Key: $KEY" -H "$J" -d "$BIG")"

# ==================== 5. guest read
sec "5. guest read (most features work without login)"
R=$(c "$B/articles?limit=5")
ck "GET /articles 200" 200 "$(code "$B/articles?limit=5")"
# 契约：ArticlePage 无 total 字段（用 hasMore + nextPageCursor 表达"还有更多"）
NCNT=$(echo "$R" | ex 'len(d["data"]["items"])')
ck "list returns items" yes "$([ "${NCNT:-0}" -gt 0 ] && echo yes || echo no)"
ck "list has hasMore flag" yes "$(echo "$R" | ex '"yes" if "hasMore" in d["data"] else "no"')"
ck "list has syncCursor" yes "$(echo "$R" | ex '"yes" if d["data"].get("syncCursor") else "no"')"
ck "list has serverTimeMs" yes "$(echo "$R" | ex '"yes" if "serverTimeMs" in d["data"] else "no"')"
echo "     items=$NCNT hasMore=$(echo "$R" | ex 'd["data"]["hasMore"]')"
R=$(c "$B/articles/$ART_ID")
ck "GET /articles/{id} 200" 200 "$(code "$B/articles/$ART_ID")"
ck "guest: isFavorited omitted" no "$(echo "$R" | ex '"yes" if "isFavorited" in d["data"] else "no"')"
ck "title is revised version" yes "$(echo "$R" | ex '"yes" if "revised" in d["data"]["title"] else "no"')"
ck "garbage token degrades to guest" 200 "$(code -H "Authorization: Bearer not-a-jwt" "$B/articles/$ART_ID")"
ck "well-formed bad-sig token degrades" 200 "$(code -H "Authorization: Bearer aaa.bbb.ccc" "$B/articles/$ART_ID")"
ck "GET /sources 200" 200 "$(code "$B/sources")"
ck "GET /categories 200" 200 "$(code "$B/categories")"
# 契约：文章尚未合成音频时 GET /articles/{id}/audio 返回 404 NOT_FOUND
# （"该文章尚未合成音频"），这是正常业务态而非错误——游客在合成前查询就该是这个结果。
ck "GET /articles/{id}/audio (guest, not synthesized)" 404 "$(code "$B/articles/$ART_ID/audio")"
ck "un-synthesized audio -> NOT_FOUND" NOT_FOUND "$(c "$B/articles/$ART_ID/audio" | ex 'd["code"]')"
ck "id=0 -> 4xx" yes "$(is4 "$(code "$B/articles/0")")"
ck "nonexistent id -> 4xx" yes "$(is4 "$(code "$B/articles/999999999")")"
ck "non-numeric id -> 4xx" yes "$(is4 "$(code "$B/articles/abc")")"

# ==================== 6. cursor
sec "6. incremental cursor (syncCursor)"
R=$(c "$B/articles?limit=2")
CUR=$(echo "$R" | ex 'd["data"]["syncCursor"]')
echo "     syncCursor=${CUR:0:44}..."
ck "first page returns syncCursor" yes "$([ -n "$CUR" ] && echo yes || echo no)"
ck "replay same cursor -> empty delta" 0 "$(c "$B/articles?limit=10&cursor=$CUR" | ex 'len(d["data"]["items"])')"
c -X POST "$B/ingest/articles" -H "X-Api-Key: $KEY" -H "$J" -d "{\"sourceKey\":\"$SRC_KEY\",\"externalId\":\"e-n$RN\",\"url\":\"https://s.example.com/n$RN\",\"title\":\"Incremental $RN\",\"publishedAt\":\"$T3\"}" >/dev/null
sleep 2
ck "delta picks up new article" yes "$([ "$(c "$B/articles?limit=10&cursor=$CUR" | ex 'len(d["data"]["items"])')" -gt 0 ] && echo yes || echo no)"
ck "garbage cursor -> 4xx" yes "$(is4 "$(code "$B/articles?cursor=@@@bogus@@@")")"
# limit: queryInt enforces [min,max]; out-of-range IS 400 by design
ck "limit=999999 -> 400 (by design)" 400 "$(code "$B/articles?limit=999999")"
ck "limit=-5 -> 400 (by design)" 400 "$(code "$B/articles?limit=-5")"
ck "limit=abc -> 400 (by design)" 400 "$(code "$B/articles?limit=abc")"
ck "limit=0 -> 400 (by design)" 400 "$(code "$B/articles?limit=0")"
ck "limit=200 (max) -> 200" 200 "$(code "$B/articles?limit=200")"

# ==================== 7. auth-required rejects guest
sec "7. auth-required endpoints reject guest"
for p in /me /me/favorites /me/reads /me/preferences /me/sessions; do
  ck "guest GET $p -> 401" 401 "$(code "$B$p")"
done
ck "guest POST /me/merge -> 401" 401 "$(code -X POST "$B/me/merge" -H "$J" -d '{"clientId":"x","nonce":"y"}')"

# ==================== 8. register / login
sec "8. register & login (register == login, saves one Argon2id)"
R=$(c -X POST "$B/auth/register" -H "$J" -d "{\"username\":\"$U\",\"password\":\"$PW\",\"clientId\":\"$CID\"}")
ck "register ok" OK "$(echo "$R" | ex 'd["code"]')"
ACC=$(echo "$R" | ex 'd["data"]["accessToken"]')
REF=$(echo "$R" | ex 'd["data"]["refreshToken"]')
USER_ID=$(echo "$R" | ex 'd["data"]["user"]["id"]')
ck "returns accessToken" yes "$([ -n "$ACC" ] && echo yes || echo no)"
ck "returns refreshToken" yes "$([ -n "$REF" ] && echo yes || echo no)"
ck "returns user object" yes "$(echo "$R" | ex '"yes" if "user" in d["data"] else "no"')"
ck "Set-Cookie refresh present" yes "$(c -i -X POST "$B/auth/register" -H "$J" -d "{\"username\":\"${U}x\",\"password\":\"$PW\",\"clientId\":\"$CID\"}" | tr -d '\r' | grep -qi 'set-cookie:.*refresh' && echo yes || echo no)"
ck "Cookie is HttpOnly" yes "$(c -i -X POST "$B/auth/register" -H "$J" -d "{\"username\":\"${U}y\",\"password\":\"$PW\",\"clientId\":\"$CID\"}" | tr -d '\r' | grep -qi 'httponly' && echo yes || echo no)"
echo "     userId=$USER_ID"
ck "duplicate username -> 4xx" yes "$(is4 "$(code -X POST "$B/auth/register" -H "$J" -d "{\"username\":\"$U\",\"password\":\"$PW\",\"clientId\":\"x\"}")")"
ck "weak password -> 4xx" yes "$(is4 "$(code -X POST "$B/auth/register" -H "$J" -d "{\"username\":\"weak$RN\",\"password\":\"123\",\"clientId\":\"x\"}")")"
ck "short username -> 4xx" yes "$(is4 "$(code -X POST "$B/auth/register" -H "$J" -d "{\"username\":\"ab\",\"password\":\"$PW\",\"clientId\":\"x\"}")")"
ck "wrong password -> 401" 401 "$(code -X POST "$B/auth/login" -H "$J" -d "{\"username\":\"$U\",\"password\":\"WrongOne1!\",\"clientId\":\"x\"}")"
ck "unknown user -> 401" 401 "$(code -X POST "$B/auth/login" -H "$J" -d "{\"username\":\"nobody$RN\",\"password\":\"$PW\",\"clientId\":\"x\"}")"
R=$(c -X POST "$B/auth/login" -H "$J" -d "{\"username\":\"$U\",\"password\":\"$PW\",\"clientId\":\"$CID\"}")
ck "login ok" OK "$(echo "$R" | ex 'd["code"]')"
AU="Authorization: Bearer $(echo "$R" | ex 'd["data"]["accessToken"]')"
ck "login issues new token" yes "$([ "$(echo "$R" | ex 'd["data"]["accessToken"]')" != "$ACC" ] && echo yes || echo no)"

# ==================== 9. logged-in features
sec "9. favorites / reads / preferences / profile"
ck "GET /me 200" 200 "$(code -H "$AU" "$B/me")"
ck "POST /me/favorites" 200 "$(code -X POST -H "$AU" -H "$J" -d "{\"articleId\":$ART_ID,\"deleted\":false}" "$B/me/favorites")"
ck "POST /me/reads" 200 "$(code -X POST -H "$AU" -H "$J" -d "{\"articleId\":$ART_ID,\"deleted\":false}" "$B/me/reads")"
ck "PUT /me/preferences" 200 "$(code -X PUT -H "$AU" -H "$J" -d '{"preferences":{"tts.voice":"101001","tts.speed":"1.2","ui.density":"compact"}}' "$B/me/preferences")"
ck "prefs roundtrip" 101001 "$(c -H "$AU" "$B/me/preferences" | ex 'd["data"]["preferences"]["tts.voice"]')"
ck "prefs key count=3" 3 "$(c -H "$AU" "$B/me/preferences" | ex 'len(d["data"]["preferences"])')"
ck "prefs replace (not merge)" 200 "$(code -X PUT -H "$AU" -H "$J" -d '{"preferences":{"tts.voice":"101002"}}' "$B/me/preferences")"
ck "prefs replaced to 1 key" 1 "$(c -H "$AU" "$B/me/preferences" | ex 'len(d["data"]["preferences"])')"
R=$(c -H "$AU" "$B/me/favorites")
ck "favorite stored" yes "$(echo "$R" | ex "\"yes\" if any(i['articleId']==$ART_ID for i in d['data']['items']) else \"no\"")"
ck "favorite has title (new field)" yes "$(echo "$R" | ex "\"yes\" if any(i['articleId']==$ART_ID and i.get('title') for i in d['data']['items']) else \"no\"")"
ck "favorite has sourceName" yes "$(echo "$R" | ex "\"yes\" if any(i['articleId']==$ART_ID and i.get('sourceName') for i in d['data']['items']) else \"no\"")"
ck "favorite updatedAt is int ms" yes "$(echo "$R" | ex "'yes' if all(isinstance(i['updatedAt'],int) for i in d['data']['items']) else 'no'")"
ck "withArticles=false still works" 200 "$(code -H "$AU" "$B/me/favorites?withArticles=false")"
ck "withArticles=false drops title" no "$(c -H "$AU" "$B/me/favorites?withArticles=false" | ex "'yes' if any('title' in i for i in d['data']['items']) else 'no'")"
R=$(c -H "$AU" "$B/articles/$ART_ID")
ck "logged-in isFavorited=true" True "$(echo "$R" | ex 'd["data"].get("isFavorited")')"
ck "logged-in isRead=true" True "$(echo "$R" | ex 'd["data"].get("isRead")')"
c -X POST -H "$AU" -H "$J" -d "{\"articleId\":$ART_ID,\"deleted\":true}" "$B/me/favorites" >/dev/null
ck "un-favorite creates tombstone" True "$(c -H "$AU" "$B/me/favorites" | ex "any(i['articleId']==$ART_ID and i['deleted'] for i in d['data']['items'])")"
ck "tombstone present in default delta" True "$(c -H "$AU" "$B/me/favorites" | ex "any(i['articleId']==$ART_ID and i['deleted'] for i in d['data']['items'])")"
ck "since=0 is not a valid cursor -> 4xx" yes "$(is4 "$(code -H "$AU" "$B/me/favorites?since=0")")"
ck "empty since (full page) -> 200" 200 "$(code -H "$AU" "$B/me/favorites?since=")"
ck "guest sees no other-user state" no "$(c "$B/articles/$ART_ID" | ex '"yes" if "isFavorited" in d["data"] else "no"')"
ck "favorites limit clamped not 500" 200 "$(code -H "$AU" "$B/me/favorites?limit=1000")"
ck "favorites limit 9999 -> 400" 400 "$(code -H "$AU" "$B/me/favorites?limit=9999")"
ck "favorite nonexistent article -> 4xx" yes "$(is4 "$(code -X POST -H "$AU" -H "$J" -d '{"articleId":999999999,"deleted":false}' "$B/me/favorites")")"

# ==================== 9b. batch state
sec "9b. batch set state (all-or-nothing, max 500)"
# 用 3b 段建出来的「干净」文章：CAT_ID/CAT_Y/CAT_Z 在本脚本里从未被标记过
# 已读或收藏。必须用干净文章，否则「库里没留下行」这条断言不可判定 ——
# 拿第 9 段已标记过的 ART_ID 来试，它本来就有行，出错也看不出是否部分提交。
R=$(c -X POST -H "$AU" -H "$J" -d "{\"articleIds\":[$CAT_ID,$CAT_Y],\"read\":true}" "$B/me/state/batch")
ck "batch mark read ok" OK "$(echo "$R" | ex 'd["code"]')"
ck "batch readsChanged=2" 2 "$(echo "$R" | ex 'd["data"]["readsChanged"]')"
ck "batch favoritesChanged=0 when favorite omitted" 0 "$(echo "$R" | ex 'd["data"]["favoritesChanged"]')"
ck "batch wrote both rows" 2 "$(c -H "$AU" "$B/me/reads" | ex "len([i for i in d['data']['items'] if i['articleId'] in ($CAT_ID,$CAT_Y) and not i['deleted']])")"
ck "batch marks article isRead" True "$(c -H "$AU" "$B/articles/$CAT_ID" | ex 'd["data"].get("isRead")')"
# favorite 省略 = 不改动收藏：这次收藏增量里不该出现 CAT_ID/CAT_Y
ck "★favorite omitted leaves favorites untouched" 0 "$(c -H "$AU" "$B/me/favorites" | ex "len([i for i in d['data']['items'] if i['articleId'] in ($CAT_ID,$CAT_Y)])")"
# favorite 显式 true 才写收藏
R=$(c -X POST -H "$AU" -H "$J" -d "{\"articleIds\":[$CAT_ID],\"read\":true,\"favorite\":true}" "$B/me/state/batch")
ck "batch favorite=true -> favoritesChanged=1" 1 "$(echo "$R" | ex 'd["data"]["favoritesChanged"]')"
ck "batch favorite=true persisted" True "$(c -H "$AU" "$B/articles/$CAT_ID" | ex 'd["data"].get("isFavorited")')"
# 幂等：重放同一批次，计数不变、行不翻倍
R=$(c -X POST -H "$AU" -H "$J" -d "{\"articleIds\":[$CAT_ID,$CAT_Y],\"read\":true}" "$B/me/state/batch")
ck "★batch replay is idempotent" 2 "$(c -H "$AU" "$B/me/reads" | ex "len([i for i in d['data']['items'] if i['articleId'] in ($CAT_ID,$CAT_Y)])")"
# 重复 id 去重：同一篇文章写两次仍只算一条
R=$(c -X POST -H "$AU" -H "$J" -d "{\"articleIds\":[$CAT_Z,$CAT_Z,$CAT_Z],\"read\":true}" "$B/me/state/batch")
ck "duplicate ids deduped -> readsChanged=1" 1 "$(echo "$R" | ex 'd["data"]["readsChanged"]')"

# ---- 原子性：一条不存在则整批不落库 ----
# ★ 这是刚修好的那个 bug 的端到端回归（原来逐条autocommit，第 2 条失败时
#   第 1 条已提交且无法回滚）。CAT_Z 此刻已有 read 行，所以额外用 CAT_Y 做对照不行 ——
#   改用 3b 段的文章重新走一遍：先确认它没有 read 行，再发一个含非法 id 的批次，
#   之后断言它**依然**没有 read 行。后半句才是本断言的真正内容。
R=$(ING '{"sourceKey":"'$SRC_KEY'","externalId":"cat-atomic'$RN'","url":"https://smoke.example.com/catatomic'$RN'","title":"Atomicity Probe'$RN'","publishedAt":"'$T1'"}')
ATOM=$(echo "$R" | ex 'd["data"]["results"][0]["articleId"]')
ck "atomicity probe has no read row before" 0 "$(c -H "$AU" "$B/me/reads" | ex "len([i for i in d['data']['items'] if i['articleId']==$ATOM])")"
ck "batch with nonexistent id -> 4xx" yes "$(is4 "$(code -X POST -H "$AU" -H "$J" -d "{\"articleIds\":[$ATOM,999999999],\"read\":true,\"favorite\":true}" "$B/me/state/batch")")"
ck "★failed batch left NO read row" 0 "$(c -H "$AU" "$B/me/reads" | ex "len([i for i in d['data']['items'] if i['articleId']==$ATOM])")"
ck "★failed batch left NO favorite row" 0 "$(c -H "$AU" "$B/me/favorites" | ex "len([i for i in d['data']['items'] if i['articleId']==$ATOM])")"
ck "★failed batch did not mark isRead" False "$(c -H "$AU" "$B/articles/$ATOM" | ex 'd["data"].get("isRead")')"
# 整批失败后去掉非法 id 重放同一批 → 必须成功（证明上面确实没留下半成品）
R=$(c -X POST -H "$AU" -H "$J" -d "{\"articleIds\":[$ATOM],\"read\":true}" "$B/me/state/batch")
ck "★replay after failure succeeds" 1 "$(echo "$R" | ex 'd["data"]["readsChanged"]')"

# ---- 上限与鉴权 ----
# 用 id 1..501：这批 id 绝大多数**不存在**，所以若上限检查排在存在性校验之后，
# 返回的会是 404 而不是 413。这条断言因此同时锁住了「上限=500」与
# 「上限判定在存在性查询之前」两件事（顺序错了就变成 404 而挂）。
B501=$($PY -c "print(','.join(str(i) for i in range(1,502)))")
ck "501 ids -> 413 (not 404: size checked first)" 413 "$(code -X POST -H "$AU" -H "$J" -d "{\"articleIds\":[$B501],\"read\":true}" "$B/me/state/batch")"
# 边界：正好 500 条必须**通过**上限判定（落到存在性校验 → 404，证明 size gate 放行）
B500=$($PY -c "print(','.join(str(i) for i in range(1,501)))")
ck "exactly 500 ids passes size gate (-> 404)" 404 "$(code -X POST -H "$AU" -H "$J" -d "{\"articleIds\":[$B500],\"read\":true}" "$B/me/state/batch")"
ck "missing read -> 4xx" yes "$(is4 "$(code -X POST -H "$AU" -H "$J" -d "{\"articleIds\":[$CAT_ID]}" "$B/me/state/batch")")"
ck "empty articleIds -> 4xx" yes "$(is4 "$(code -X POST -H "$AU" -H "$J" -d '{"articleIds":[],"read":true}' "$B/me/state/batch")")"
ck "articleId=0 -> 4xx" yes "$(is4 "$(code -X POST -H "$AU" -H "$J" -d '{"articleIds":[0],"read":true}' "$B/me/state/batch")")"
ck "guest POST /me/state/batch -> 401" 401 "$(code -X POST -H "$J" -d "{\"articleIds\":[$CAT_ID],\"read\":true}" "$B/me/state/batch")"

# ==================== 10. TTS (mock)
sec "10. server-side TTS full chain (mock provider)"
# 契约：POST /audio/tasks 返回 task（id + status）；状态轮询走 GET /audio/tasks/{taskId}；
# 成品查询走 GET /articles/{id}/audio（AudioBriefDTO：只有 url/format/durationMs/sizeBytes，
# **没有 audioId 字段**，audioId 要从 url 末段解析）。
# ⚠️ 这里只创建一次任务：重复 POST 是幂等命中缓存，会直接返回 ready，
#    "initial status is pending/processing" 断言反而会挂。
R=$(c -X POST -H "$AU" -H "$J" -d "{\"articleId\":$ART_ID}" "$B/audio/tasks")
TASK_ID=$(echo "$R" | ex 'd["data"]["id"] or d["data"].get("taskId")')
INIT_ST=$(echo "$R" | ex 'd["data"]["status"]')
echo "     taskId=$TASK_ID initialStatus=$INIT_ST"
ck "initial status is pending/processing" yes "$(case "$INIT_ST" in pending|processing) echo yes;; *) echo "no:$INIT_ST";; esac)"
ST="$INIT_ST"
for i in $(seq 1 20); do
  if [ "$ST" = "ready" ] || [ "$ST" = "failed" ]; then break; fi
  sleep 1
  R=$(c -H "$AU" "$B/audio/tasks/$TASK_ID")
  ST=$(echo "$R" | ex 'd["data"]["status"]')
done
echo "     final status=$ST"
ck "TTS reaches ready" ready "$ST"
R=$(c -H "$AU" "$B/articles/$ART_ID/audio")
AURL=$(echo "$R" | ex 'd["data"].get("url") or ""')
# AudioBriefDTO 无 audioId 字段，从 url（/api/v1/audio/{id}）末段取
AID=$(echo "$AURL" | sed -n 's#.*/##p')
ck "article audio has url" yes "$([ -n "$AURL" ] && echo yes || echo no)"
echo "     url=$AURL audioId=$AID"
ck "GET /audio/{id} 200" 200 "$(code -H "$AU" "$B/audio/$AID")"
SZ=$(c -o /dev/null -w '%{size_download}' -H "$AU" "$B/audio/$AID")
CT=$(c -o /dev/null -w '%{content_type}' -H "$AU" "$B/audio/$AID")
echo "     bytes=$SZ contentType=$CT"
ck "audio non-empty (>1KB)" yes "$([ "${SZ:-0}" -gt 1024 ] && echo yes || echo no)"
ck "audio has audio content-type" yes "$(case "$CT" in audio/*) echo yes;; *) echo "no:$CT";; esac)"
# 幂等：同一 (article,voice,speed) 再次请求应命中缓存并直接 ready，不重复排队。
ck "re-request is idempotent (cache hit) -> ready" ready "$(c -X POST -H "$AU" -H "$J" -d "{\"articleId\":$ART_ID}" "$B/audio/tasks" | ex 'd["data"]["status"]')"
ck "task for bad article -> 4xx" yes "$(is4 "$(code -X POST -H "$AU" -H "$J" -d '{"articleId":999999999}' "$B/audio/tasks")")"
ck "unknown taskId -> 4xx" yes "$(is4 "$(code -H "$AU" "$B/audio/tasks/999999")")"
# 游客读音频：音频文件本身不设登录墙（与"游客可用大部分功能"一致）
ck "guest can read audio (no login wall)" 200 "$(code "$B/audio/$AID")"

# ==================== 11. DEC-7
sec "11. DEC-7: rotate / grace-replay / leak detection"
# 每次用独立账号，避免前一个用例的撤销影响后一个用例的判定。
mkuser() { # mkuser <tag> -> "ACC REF"
  local t="$1"
  local un="d7${t}$RN"
  local r
  r=$(c -X POST "$B/auth/register" -H "$J" -d "{\"username\":\"$un\",\"password\":\"$PW\",\"clientId\":\"cid-$t-$RN\"}")
  echo "$(echo "$r" | ex 'd["data"]["accessToken"]') $(echo "$r" | ex 'd["data"]["refreshToken"]')"
}

# --- A 正常轮换 ---
read -r K_ACC K_REF <<< "$(mkuser A)"
R=$(c -X POST "$B/auth/refresh" -H "$J" -d "{\"refreshToken\":\"$K_REF\",\"clientId\":\"cid-A-$RN\"}")
ck "A normal rotate" OK "$(echo "$R" | ex 'd["code"]')"
K_REF2=$(echo "$R" | ex 'd["data"]["refreshToken"]')
K_ACC2=$(echo "$R" | ex 'd["data"]["accessToken"]')
ck "A issues new refreshToken" yes "$([ -n "$K_REF2" ] && [ "$K_REF2" != "$K_REF" ] && echo yes || echo no)"
ck "A refresh returns user object" yes "$(echo "$R" | ex '"yes" if "user" in d["data"] else "no"')"
ck "A new access works" 200 "$(code -H "Authorization: Bearer $K_ACC2" "$B/me/favorites")"

# --- C 宽限重放：prev 命中 + 同 clientId + 窗口内 → 幂等再轮换，不踢设备 ---
R=$(c -X POST "$B/auth/refresh" -H "$J" -d "{\"refreshToken\":\"$K_REF\",\"clientId\":\"cid-A-$RN\"}")
ck "C grace replay same device -> OK" OK "$(echo "$R" | ex 'd["code"]')"
ck "C did not kill the session" 200 "$(code -H "Authorization: Bearer $K_ACC2" "$B/me/favorites")"
K_REF3=$(echo "$R" | ex 'd["data"]["refreshToken"]')
ck "C newest refresh still valid" OK "$(c -X POST "$B/auth/refresh" -H "$J" -d "{\"refreshToken\":\"$K_REF3\",\"clientId\":\"cid-A-$RN\"}" | ex 'd["code"]')"

# --- D 泄露：prev 命中但异设备 → 撤销全部会话 ---
read -r D_ACC D_REF <<< "$(mkuser D)"
read -r D_SIB_ACC D_SIB_REF <<< "$(mkuser D2)"
R=$(c -X POST "$B/auth/refresh" -H "$J" -d "{\"refreshToken\":\"$D_REF\",\"clientId\":\"cid-D-$RN\"}")
D_REF2=$(echo "$R" | ex 'd["data"]["refreshToken"]')
D_ACC2=$(echo "$R" | ex 'd["data"]["accessToken"]')
R=$(c -X POST "$B/auth/refresh" -H "$J" -d "{\"refreshToken\":\"$D_REF\",\"clientId\":\"attacker-$RN\"}")
RC=$(echo "$R" | ex 'd["code"]')
ck "D prev token from other device -> rejected" yes "$([ "$RC" != "OK" ] && echo yes || echo no)"
echo "     code=$RC"
ck "★D leak revokes ALL access tokens" 401 "$(code -H "Authorization: Bearer $D_ACC2" "$B/me/favorites")"
ck "★D leak revokes newest refresh" yes "$([ "$(c -X POST "$B/auth/refresh" -H "$J" -d "{\"refreshToken\":\"$D_REF2\",\"clientId\":\"cid-D-$RN\"}" | ex 'd["code"]')" != "OK" ] && echo yes || echo no)"
# 但 D2 是另一个账号，不应受影响
ck "D leak does NOT affect other users" 200 "$(code -H "Authorization: Bearer $D_SIB_ACC" "$B/me/favorites")"
# 同一用户的多设备必须一起死 —— 用 D 的第二个会话验证
R=$(c -X POST "$B/auth/login" -H "$J" -d "{\"username\":\"d7D$RN\",\"password\":\"$PW\",\"clientId\":\"cid-D2-$RN\"}")
D2_ACC=$(echo "$R" | ex 'd["data"]["accessToken"]')
ck "same-user 2nd session alive pre-leak" 200 "$(code -H "Authorization: Bearer $D2_ACC" "$B/me/favorites")"
c -X POST "$B/auth/refresh" -H "$J" -d "{\"refreshToken\":\"$D_REF2\",\"clientId\":\"attacker-$RN\"}" >/dev/null
ck "★same-user 2nd session killed by leak" 401 "$(code -H "Authorization: Bearer $D2_ACC" "$B/me/favorites")"

# --- 其他 refresh 边界 ---
ck "missing clientId on valid token -> rejected" yes "$([ "$(c -X POST "$B/auth/refresh" -H "$J" -d "{\"refreshToken\":\"$K_REF3\"}" | ex 'd["code"]')" != "OK" ] && echo yes || echo no)"
ck "garbage refresh -> 401" 401 "$(code -X POST "$B/auth/refresh" -H "$J" -d '{"refreshToken":"garbage","clientId":"x"}')"
ck "empty refresh -> 401" 401 "$(code -X POST "$B/auth/refresh" -H "$J" -d '{"refreshToken":"","clientId":"x"}')"
ck "no body -> 401" 401 "$(code -X POST "$B/auth/refresh" -H "$J" -d '{}')"

# ==================== 12. logout
sec "12. logout must really destroy the session"
R=$(c -X POST "$B/auth/login" -H "$J" -d "{\"username\":\"$U\",\"password\":\"$PW\",\"clientId\":\"cli-out-$RN\"}")
LREF=$(echo "$R" | ex 'd["data"]["refreshToken"]')
LACC=$(echo "$R" | ex 'd["data"]["accessToken"]')
ck "access works before logout" 200 "$(code -H "Authorization: Bearer $LACC" "$B/me/favorites")"
ck "POST /auth/logout -> 204" 204 "$(code -X POST "$B/auth/logout" -H "$J" -d "{\"refreshToken\":\"$LREF\"}")"
RC=$(c -X POST "$B/auth/refresh" -H "$J" -d "{\"refreshToken\":\"$LREF\",\"clientId\":\"cli-out-$RN\"}" | ex 'd["code"]')
ck "★★old refresh MUST fail after logout" yes "$([ "$RC" != "OK" ] && echo yes || echo no)"
echo "     code=$RC"
ck "access dead after logout" 401 "$(code -H "Authorization: Bearer $LACC" "$B/me/favorites")"
ck "repeat logout idempotent -> 204" 204 "$(code -X POST "$B/auth/logout" -H "$J" -d "{\"refreshToken\":\"$LREF\"}")"
ck "logout with no creds -> 204" 204 "$(code -X POST "$B/auth/logout" -H "$J" -d '{}')"
# cookie-channel logout
CK_="$SMOKE_COOKIE_JAR"
rm -f "$CK_"
c -c "$CK_" -X POST "$B/auth/login" -H "$J" -d "{\"username\":\"$U\",\"password\":\"$PW\",\"clientId\":\"cli-ck-$RN\"}" >/dev/null
ck "cookie-channel logout -> 204" 204 "$(c -b "$CK_" -o /dev/null -w '%{http_code}' -X POST "$B/auth/logout" -H "$J" -d '{}')"
ck "cookie refresh gone after logout" yes "$([ "$(c -b "$CK_" -X POST "$B/auth/refresh" -H "$J" -d "{\"clientId\":\"cli-ck-$RN\"}" | ex 'd["code"]')" != "OK" ] && echo yes || echo no)"

# ==================== 13. merge
sec "13. merge: guest-state merge + nonce idempotency + clock clamp"
R=$(c -X POST "$B/auth/login" -H "$J" -d "{\"username\":\"$U\",\"password\":\"$PW\",\"clientId\":\"$CID\"}")
AU="Authorization: Bearer $(echo "$R" | ex 'd["data"]["accessToken"]')"
NONCE="n-$RN-$NOW_MS"
M="{\"clientId\":\"$CID\",\"nonce\":\"$NONCE\",\"favorites\":[{\"articleId\":$ART_ID,\"deleted\":false,\"updatedAt\":$NOW_MS}],\"reads\":[{\"articleId\":$ART_ID,\"deleted\":false,\"updatedAt\":$NOW_MS}],\"preferences\":{\"tts.voice\":\"mock-voice\"}}"
R=$(c -X POST "$B/me/merge" -H "$AU" -H "$J" -d "$M")
ck "merge ok" OK "$(echo "$R" | ex 'd["code"]')"
ck "first applied=true" True "$(echo "$R" | ex 'd["data"].get("applied")')"
R2=$(c -X POST "$B/me/merge" -H "$AU" -H "$J" -d "$M")
ck "same nonce -> replayed=true" True "$(echo "$R2" | ex 'd["data"].get("replayed")')"
ck "★replay does not re-apply" False "$(echo "$R2" | ex 'd["data"].get("applied")')"
R3=$(c -X POST "$B/me/merge" -H "$AU" -H "$J" -d "$M")
ck "★replay result identical" "$(echo "$R2" | ex 'json.dumps(d["data"],sort_keys=True,default=str)')" "$(echo "$R3" | ex 'json.dumps(d["data"],sort_keys=True,default=str)')"
ck "far-future updatedAt clamped, no error" OK "$(c -X POST "$B/me/merge" -H "$AU" -H "$J" -d "{\"clientId\":\"$CID\",\"nonce\":\"n-clamp-$RN\",\"favorites\":[{\"articleId\":$ART_ID,\"deleted\":false,\"updatedAt\":99999999999999}]}" | ex 'd["code"]')"
ck "updatedAt=0 rejected (explicit contract)" VALIDATION_FAILED "$(c -X POST "$B/me/merge" -H "$AU" -H "$J" -d "{\"clientId\":\"$CID\",\"nonce\":\"n-zero-$RN\",\"favorites\":[{\"articleId\":$ART_ID,\"deleted\":false,\"updatedAt\":0}]}" | ex 'd["code"]')"
ck "missing nonce -> 4xx" yes "$(is4 "$(code -X POST "$B/me/merge" -H "$AU" -H "$J" -d "{\"clientId\":\"$CID\",\"favorites\":[]}")")"
ck "missing clientId -> 4xx" yes "$(is4 "$(code -X POST "$B/me/merge" -H "$AU" -H "$J" -d "{\"nonce\":\"x-$RN\",\"favorites\":[]}")")"
ck "LWW: older timestamp does not win" yes "$(c -X POST "$B/me/merge" -H "$AU" -H "$J" -d "{\"clientId\":\"$CID\",\"nonce\":\"n-lww-$RN\",\"favorites\":[{\"articleId\":$ART_ID,\"deleted\":true,\"updatedAt\":1000}]}" | ex "'yes' if d['data']['favorites']['skipped']>0 else 'no'")"
ck "merge persists favorite" yes "$(c -H "$AU" "$B/me/favorites" | ex "\"yes\" if any(i['articleId']==$ART_ID and not i['deleted'] for i in d['data']['items']) else \"no\"")"

# ==================== 14. password change
sec "14. change password (revoke all + reissue for current device)"
R=$(c -X POST "$B/auth/login" -H "$J" -d "{\"username\":\"$U\",\"password\":\"$PW\",\"clientId\":\"cli-pw-$RN\"}")
P_ACC=$(echo "$R" | ex 'd["data"]["accessToken"]')
P_REF=$(echo "$R" | ex 'd["data"]["refreshToken"]')
R=$(c -X POST "$B/auth/login" -H "$J" -d "{\"username\":\"$U\",\"password\":\"$PW\",\"clientId\":\"cli-pw2-$RN\"}")
OTH_ACC=$(echo "$R" | ex 'd["data"]["accessToken"]')
R=$(c -X PATCH -H "Authorization: Bearer $P_ACC" -H "$J" -d "{\"oldPassword\":\"$PW\",\"newPassword\":\"$NPW\"}" "$B/me/password")
ck "change ok" OK "$(echo "$R" | ex 'd["code"]')"
N_ACC=$(echo "$R" | ex 'd["data"].get("accessToken")')
N_REF=$(echo "$R" | ex 'd["data"].get("refreshToken")')
ck "★not kicked out of own change" yes "$([ -n "$N_ACC" ] && [ -n "$N_REF" ] && echo yes || echo no)"
ck "reissued access works" 200 "$(code -H "Authorization: Bearer $N_ACC" "$B/me/favorites")"
ck "★other device kicked" 401 "$(code -H "Authorization: Bearer $OTH_ACC" "$B/me/favorites")"
ck "old refresh dead" yes "$([ "$(c -X POST "$B/auth/refresh" -H "$J" -d "{\"refreshToken\":\"$P_REF\",\"clientId\":\"cli-pw-$RN\"}" | ex 'd["code"]')" != "OK" ] && echo yes || echo no)"
ck "old password rejected" 401 "$(code -X POST "$B/auth/login" -H "$J" -d "{\"username\":\"$U\",\"password\":\"$PW\",\"clientId\":\"x\"}")"
ck "new password works" 200 "$(code -X POST "$B/auth/login" -H "$J" -d "{\"username\":\"$U\",\"password\":\"$NPW\",\"clientId\":\"x\"}")"
ck "wrong oldPassword -> 4xx" yes "$(is4 "$(code -X PATCH -H "Authorization: Bearer $N_ACC" -H "$J" -d "{\"oldPassword\":\"nope\",\"newPassword\":\"$NPW\"}" "$B/me/password")")"
ck "empty newPassword -> 4xx" yes "$(is4 "$(code -X PATCH -H "Authorization: Bearer $N_ACC" -H "$J" -d "{\"oldPassword\":\"$NPW\",\"newPassword\":\"\"}" "$B/me/password")")"

# ==================== 15. sessions / search / sources
sec "15. sessions, search, source management"
# 用改密后新签的 token（改密已撤销全部旧会话，旧 token 必然 401）
# ⚠️ clientId 必须带双引号：写成 clientId:cli-sess-$RN 是非法 JSON，
#    服务端会正确地返回 BAD_REQUEST「请求体不是合法 JSON」，
#    后续 4 条会话断言连锁失败。值本身也要保持合法（见 clientId 约束）。
R=$(c -X POST "$B/auth/login" -H "$J" -d "{\"username\":\"$U\",\"password\":\"$NPW\",\"clientId\":\"cli-sess-$RN\"}")
# 显式断言登录成功：否则后面 4 条会话断言全挂时无法区分
# 「密码/会话有问题」与「脚本取错了变量」。
ck "re-login with new password ok" OK "$(echo "$R" | ex 'd["code"]')"
SESS_ACC=$(echo "$R" | ex 'd["data"]["accessToken"]')
SESS_LIST=$(c -H "Authorization: Bearer $SESS_ACC" "$B/me/sessions")
ck "GET /me/sessions 200" 200 "$(code -H "Authorization: Bearer $SESS_ACC" "$B/me/sessions")"
ck "sessions list has items" yes "$(echo "$SESS_LIST" | ex '"yes" if len(d["data"]["items"])>0 else "no"')"
ck "session row has clientId" yes "$(echo "$SESS_LIST" | ex '"yes" if any(s.get("clientId") for s in d["data"]["items"]) else "no"')"
# 会话吊销用「受害者会话」做，结构固定为：先登录出一个 victim 会话拿其 token，
# 再用当前 SESS_ACC 去吊销它，然后断言 victim token 失效、当前 token 仍可用。
#
# ⚠️ 踩过的坑：曾写成"吊销 clientId==cli-sess-$RN 的会话"，
#    但那恰恰就是当前会话（current:true），加了 not current 过滤后必然取到 id=0；
#    不加过滤又会把当前 token 吊销掉，导致后续所有断言 401。
#    结论：被测对象必须是**独立的第三个会话**，不要拿当前会话当靶子。
VICTIM=$(c -X POST "$B/auth/login" -H "$J" -d "{\"username\":\"$U\",\"password\":\"$NPW\",\"clientId\":\"cli-victim-$RN\"}" \
  | ex 'd["data"]["accessToken"]')
VICTIM_ID=$(c -H "Authorization: Bearer $SESS_ACC" "$B/me/sessions" \
  | ex '([s["id"] for s in d["data"]["items"] if s.get("clientId")=="cli-victim-'"$RN"'"] or [0])[0]')
echo "     revoke victimSessionId=$VICTIM_ID"
# 契约：openapi /api/v1/me/sessions/{id} delete 的成功响应是 '200'（不是 204）。
ck "revoke session by id -> 200" 200 "$(code -X DELETE -H "Authorization: Bearer $SESS_ACC" "$B/me/sessions/$VICTIM_ID")"
ck "★revoked session access dead" 401 "$(code -H "Authorization: Bearer $VICTIM" "$B/me/favorites")"
ck "current session unaffected by revoking other" 200 "$(code -H "Authorization: Bearer $SESS_ACC" "$B/me/favorites")"
ck "search 200" 200 "$(code "$B/articles?q=%E6%8A%98%E5%BD%A2%E5%B1%8F")"
ck "search empty query ok" 200 "$(code "$B/articles?q=")"
R=$(c "$B/sources")
DEF_SRC=$(echo "$R" | ex "([s['id'] for s in d['data']['items'] if s['isDefault']] or [0])[0]")
# 源管理段自建一个专用源，不复用第 0 段的 $SRC：
# 本段末尾会 DELETE 掉它，若复用则一旦本段被重跑（源已不存在），
# 后面每一条都会变成 404 并让人误判成"源管理接口坏了"。实测踩过。
MGMT=$(c -X POST "$B/sources" -H "$J" -d "{\"name\":\"Mgmt$RN\",\"url\":\"https://mgmt.example.com/rss\",\"type\":\"rss\",\"category\":\"tech\"}")
SRC=$(echo "$MGMT" | ex 'd["data"]["id"]')
SRC_KEY=$(echo "$MGMT" | ex 'd["data"]["key"]')
echo "     defaultSourceId=$DEF_SRC  mgmtSourceId=$SRC"
ck "PATCH own source enabled" 200 "$(code -X PATCH -H "$J" -d '{"enabled":false}' "$B/sources/$SRC/enabled")"
ck "disabled source marked" False "$(c "$B/sources" | ex "([s['enabled'] for s in d['data']['items'] if s['id']==$SRC] or ['?'])[0]")"
ck "PATCH re-enable" 200 "$(code -X PATCH -H "$J" -d '{"enabled":true}' "$B/sources/$SRC/enabled")"
ck "PUT own source (full body)" 200 "$(code -X PUT -H "$J" -d "{\"name\":\"Smoke Renamed\",\"url\":\"https://smoke.example.com/rss2\",\"type\":\"rss\",\"category\":\"tech\"}" "$B/sources/$SRC")"
ck "PUT rename persisted" "Smoke Renamed" "$(c "$B/sources" | ex "([s['name'] for s in d['data']['items'] if s['id']==$SRC] or ['?'])[0]")"
ck "PUT partial body -> 4xx" yes "$(is4 "$(code -X PUT -H "$J" -d '{"name":"only-name"}' "$B/sources/$SRC")")"
ck "PUT default source name -> 4xx (protected)" yes "$(is4 "$(code -X PUT -H "$J" -d "{\"name\":\"hack\",\"url\":\"https://x.example.com\",\"type\":\"rss\",\"category\":\"tech\"}" "$B/sources/$DEF_SRC")")"
ck "DELETE default source -> 4xx (protected)" yes "$(is4 "$(code -X DELETE "$B/sources/$DEF_SRC")")"
ck "same name twice -> key auto-suffixed" yes "$(c -X POST "$B/sources" -H "$J" -d "{\"name\":\"Smoke$RN\",\"url\":\"https://x.example.com/n$RN\",\"type\":\"rss\",\"category\":\"tech\"}" | ex "'yes' if d['code']=='OK' and d['data']['key']!='$SRC_KEY' else 'no:'+d['data']['key']")"
ck "missing name -> 4xx" yes "$(is4 "$(code -X POST "$B/sources" -H "$J" -d '{"url":"https://x.example.com"}')")"
ck "missing url -> 4xx" yes "$(is4 "$(code -X POST "$B/sources" -H "$J" -d '{"name":"NoUrl"}')")"
ck "non-http url -> 4xx" yes "$(is4 "$(code -X POST "$B/sources" -H "$J" -d '{"name":"Bad","url":"javascript:x"}')")"
ck "bad type enum -> 4xx" yes "$(is4 "$(code -X POST "$B/sources" -H "$J" -d '{"name":"BadType","url":"https://x.example.com","type":"telepathy"}')")"
ck "unknown source id -> 4xx" yes "$(is4 "$(code -X PATCH -H "$J" -d '{"enabled":false}' "$B/sources/999999/enabled")")"
# 契约：openapi /api/v1/sources/{id} delete 的成功响应是 '200'（不是 204）。
ck "DELETE own source -> 200" 200 "$(code -X DELETE "$B/sources/$SRC")"

# ==================== 16. envelope
sec "16. error envelope & fallbacks"
ck "unknown path -> 404" 404 "$(code "$B/nope")"
ck "root -> 404" 404 "$(code "$BASE/")"
ck "wrong method -> 4xx" yes "$(is4 "$(code -X DELETE "$B/articles/$ART_ID")")"
R=$(c "$B/nope")
ck "error has code+message" yes "$(echo "$R" | ex '"yes" if "code" in d and "message" in d else "no:"+str(list(d.keys()))')"
ck "error has requestId" yes "$(echo "$R" | ex '"yes" if "requestId" in d else "no"')"
ck "huge query string not 500" yes "$([ "$(code "$B/articles?limit=5&pad=$(printf 'a%.0s' $(seq 1 2000))")" != "500" ] && echo yes || echo no)"
ck "SQLi-ish cursor not 500" yes "$([ "$(code "$B/articles?cursor=%27%20OR%201%3D1--")" != "500" ] && echo yes || echo no)"
ck "very long path not 500" yes "$([ "$(code "$B/articles/$(printf '9%.0s' $(seq 1 3000))")" != "500" ] && echo yes || echo no)"

printf '\n\033[1m========== TOTAL: PASS=%d FAIL=%d ==========\033[0m\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ] && echo "\033[32mALL PASSED\033[0m" || echo "\033[31mHAS FAILURES\033[0m"
exit $FAIL
