# EZNews 服务端冒烟测试

端到端验收：起真实服务进程 → 打全部 HTTP 端点 → 断言契约 → 关服务。
不是单元测试，验证的是**跨层契约**（路由 / 鉴权 / DTO / SQLite / TTS 状态机）在真实进程里的行为。

## 运行

```bash
# 方式一：Makefile（会自动先 build）
cd server && make smoke

# 方式二：直接跑
cd server
go build -o /d/dev/softwares/eznews-data/bin/eznews.exe ./cmd/eznews
bash smoke/run.sh
```

环境变量：

| 变量 | 默认值 | 说明 |
|---|---|---|
| `SMOKE_BIN` | `/d/dev/softwares/eznews-data/bin/eznews.exe` | 服务端二进制路径 |
| `SMOKE_PORT` | `18080` | 测试端口（避开 8080） |

## 文件

| 文件 | 作用 |
|---|---|
| `run.sh` | 一体化：清理残留 → 查端口占用 → 起服务 → 等就绪 → 跑 `smoke.sh` → 收集服务端日志断言 → 关服务 |
| `smoke.yaml` | 测试专用配置：`tts.provider: mock`（不依赖云密钥）、独立 sqlite/audio/jwt_secret、`autoCreateSource: false` |
| `smoke.sh` | 全部断言用例（16 组） |
| `admin-e2e.sh` | **管理后台**专项端到端（64 条断言，见下） |
| `result.txt` | 最近一次运行结果 |
| `server.log` | 最近一次服务端日志（用于排查 5xx / WARN） |

## 管理后台专项：`smoke/admin-e2e.sh`

管理后台涉及"最后一个管理员"这类**不可逆**的权限边界，光靠单测不够 ——
单测通过不代表装配对了（路由漏注册、中间件顺序错、`*bool` 指针语义丢失，
这些都在单测里看不见）。所以这 64 条断言是直接打在**真实编译出来的那个二进制**上。

```bash
# 需要服务端已在 127.0.0.1:8080 跑着（config.dev.yaml），且库里有一个已知密码的管理员。
# 首次准备：
#   curl -X POST .../auth/register -d '{"username":"eztester","password":"Str0ng!Pass77","clientId":"e2e-cli"}'
#   ./bin/eznews -config config.dev.yaml -promote-admin eztester   # ← 冷启动提升，先跑迁移
cd server && bash smoke/admin-e2e.sh
```

覆盖 A–Q 共 17 组：鉴权边界 / 概览 / 列表筛选分页 / 能力位 / 自我保护 /
`*bool` 指针语义 / 停用与吊销 / 404 语义 / 代建账号 / 内容统计 / 语音任务 /
系统信息 / **降权即时生效** / **停用三入口全拦** / **最后管理员守卫** / **登录限流**。

两条容易踩的坑（脚本里已注释，此处再记一次）：

- **必须 `unset http_proxy https_proxy ...`**。本机 shell 设了 `http_proxy=127.0.0.1:60301`，
  curl 会对 `127.0.0.1` 也走这个代理，所有请求拿到 502 `upstream connect failed` ——
  报错文本会让人以为是服务端挂了。也不能用 `--noproxy '*'`：`*` 会被 bash glob 展开成
  当前目录文件名列表。
- **不要对同一账号反复登录**。`loginLimitPerUser` 是 `5/m`，脚本自己会打到 429
  （那是正确行为，但会让断言失真）。应复用已有的未过期 token —— 这也更贴近真实客户端。


## 覆盖范围

| 组 | 内容 |
|---|---|
| 0 | 显式建源（`autoCreateSource: false`，才能验证 `SOURCE_NOT_FOUND`） |
| 1 | 健康检查：`/healthz` `/readyz` `/metrics` |
| 2 | ingest 鉴权（`X-Api-Key` 与账号体系完全独立）+ 字段校验 |
| 3 | **ingest contentHash 三态**：`created` / `updated` / `skipped(UNCHANGED)` |
| 4 | 批量 ingest：逐条回执、单条失败不阻断整批、205 条 → 413 |
| 5 | 游客读：不登录可用；**带非法 token 降级为游客而非拒绝** |
| 6 | 增量游标 `syncCursor`：回放不重复、新数据可见、非法游标 4xx |
| 7 | 需登录端点拒绝游客 |
| 8 | 注册即登录（省一次 Argon2id）、Cookie 通道、弱密码/重名拒绝 |
| 9 | 收藏/已读/偏好；**登录墙**（游客看不到他人收藏态）、墓碑、批量摘要 |
| 10 | 服务端 TTS 全链路（mock provider）：创建 → 轮询 → 下载 |
| 11 | **DEC-7 refresh 四级判定**：轮换 / 宽限幂等 / 泄露撤销全量 |
| 12 | **登出真正销毁 session**（含 Cookie 通道） |
| 13 | Merge Policy v1：nonce 幂等、时钟钳制、LWW |
| 14 | 改密：撤销全部会话 + 为当前设备补发令牌 |
| 15 | 会话管理、搜索、源 CRUD（默认源受保护） |
| 16 | 错误信封、注入类输入不 500 |

## 三条不可妥协的不变量

脚本里标 `★` 的用例，失败即意味着安全/正确性回归：

1. `★★old refresh MUST fail after logout` —— 登出后用旧 refresh token 调 refresh 必须失败。
   登出形同虚设的典型症状是"用户点了退出，token 还能继续用"。
2. `★D leak revokes ALL access tokens` / `★same-user 2nd session killed by leak` ——
   refresh token 被异设备重放时，该用户**所有**会话（含其他设备）必须立即失效。
3. `★改密后不被自己踢出` —— 改密撤销全量后要为当前设备补发令牌，
   否则用户改完密码立刻被自己踢出，体验等同故障。

## 排查

`run.sh` 结束时会自动打印：服务端 `ERROR`/`WARN` 日志、所有 5xx 响应、panic 检查（应为 0）。
测试自身失败时看 `result.txt`；服务端异常看 `server.log`。

## 写断言时的坑（都踩过）

- **时间戳必须动态生成**。服务端 `publishedAt` 接受区间是 `[now-365d, now+24h]`，
  写死日期会在系统时间不同时全部 `UNPROCESSABLE`。
- **`limit` 越界返回 400 是设计如此**（`queryInt` 强制 `[min,max]`），不是 bug。
- **`ArticlePage` 没有 `total` 字段**，用 `hasMore` + `nextPageCursor` 表达"还有更多"。
- **单条 ingest 业务失败仍返回 HTTP 200**，失败明细在 `data.results[].status/reason`。
- **`SourceInput` 没有 `key` 字段**，`key` 由服务端从 `name` 自动生成（重名自动加后缀）。
- **音频状态在 `GET /audio/tasks/{taskId}`**，`GET /articles/{id}/audio` 返回的
  `AudioBriefDTO` 只有 `status/audioId/voice/speed`，没有 `status` 之外的成品 URL 字段。
- **`since` 是不透明游标**（base64url），不是数字偏移，传 `since=0` 是非法的。
