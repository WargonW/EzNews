# EZNews 开发进度

> 最后更新：2026-10-07

## 一、当前状态总览

| 模块 | 状态 | 说明 |
|---|---|---|
| 需求与架构文档 | ✅ 完成 | PRD、账号体系 PRD、架构设计（19 章）、OpenAPI 契约 |
| 服务端（Go） | ✅ 完成 | 89 个 Go 文件 / 约 25000 行；单文件部署 |
| Web 端 | ✅ 完成 | 51 个 TS/TSX 文件；阅读界面 + 管理后台 |
| Android 端 | ⬜ 未开始 | 按既定节奏后置 |
| 独立采集器 | ⬜ 未开始 | ingest 接口已就绪，等采集器接入 |

## 二、已交付内容

### 服务端

- **存储**：SQLite 单文件（`modernc.org/sqlite`，纯 Go 无 CGO），WAL + `synchronous=NORMAL`；时间统一 INTEGER epoch 毫秒，API 输出 ISO8601
- **账号体系**：
  - 口令哈希 Argon2id（m=19MiB, t=2, p=1）
  - Access Token = JWT(HS256) 2h；Refresh Token = 32B 随机串 30 天滑动续期，库内只存 SHA-256 哈希 + 轮换 + reuse 检测
  - **权限判定每次查库而非读 JWT** —— 换来降权/停用**立即生效**（本人手里的 2 小时 token 当场失效）
  - 停用拦截三入口齐备：access 校验 / 登录 / refresh
  - 401 与 403 严格区分（可重放 vs 立即跳出登录态）
- **管理后台 API**：12 个端点，覆盖用户 / 新闻源 / 语音任务 / 内容统计 / 系统信息
  - 含「最后一个可用管理员守卫」，防止误操作把唯一的 admin 降级/停用/删除致系统锁死
  - 能力位（`canChangeRole` 等）由服务端下发，避免规则前后端各写一遍而漂移
- **TTS**：`TtsProvider` 抽象 + 表驱动状态机 `pending→processing→ready|failed`，worker 池，缓存键 `(article_id, voice, speed)`；已实现 mock / 阿里 / 腾讯（讯飞未实现）
- **检索**：FTS5 全文检索；增量游标复合键 `(updated_at, id)`，行值比较
- **去重三态**：`created` / `updated` / `skipped(UNCHANGED)`。内容无变化时只刷 `last_seen_at` 而不刷 `updated_at`，否则周期全量重投会让增量退化成全量
- **限流**：无 Redis，5 层进程内保护（请求体上限 → 全局令牌桶 → key 令牌桶 → 批量条数上限 → 全局写互斥锁）
- **交付形态**：`//go:embed all:dist` 内嵌前端，产出单个可执行文件（约 13 MB）

### Web 端

- **信息流**：左侧点选来源 → 右侧整体切换；点具体来源只看该源新闻
- **隐藏源**：全量流下的高级筛选（默认折叠），随偏好持久化并跨设备同步
- **游客可用**：不登录可浏览、播放、本地收藏；登录后收藏/已读/偏好云端同步（含游客数据合并的 LWW 策略与墓碑机制）
- **管理后台** `/admin`：概览 / 用户 / 新闻源 / 语音任务 / 内容 / 系统 六个分区
- **语音播放**：服务端合成、客户端播放，含续播与语速音色偏好

### 文档

| 文件 | 内容 |
|---|---|
| `docs/PRD.md` | 产品需求 |
| `docs/PRD-ACCOUNT.md` | 账号体系需求 |
| `docs/ARCHITECTURE.md` | 系统架构，19 章 |
| `docs/api/openapi.yaml` | API 契约（**权威**，实现与它冲突时以它为准） |

## 三、质量保障

- **服务端**：`gofmt` / `go vet` / `go test ./...` 全绿；含 repo 层 SQL 回归网、auth 停用语义、admin 服务层测试
- **Web 端**：`tsc -b --force` / `eslint --max-warnings 0` / `vitest` 全绿（当前 **91 例**）
- **端到端**：`server/smoke/` 下两套脚本打在**真实编译出的二进制**上
  - `run.sh` —— 主流程冒烟
  - `admin-e2e.sh` —— 管理后台专项，64 条断言 / 17 组
- **真实浏览器复验**：关键交互改动均用 Chrome CDP 驱动真实浏览器验证，而非只跑单测

## 四、下一步计划

### 近期（建议优先级）

1. **独立采集器工具** —— 服务端 ingest 接口已就绪，但**目前没有任何东西往里写数据**，这是当前最大的功能缺口。计划做一个独立 CLI/服务，按源配置定时抓取 RSS/Atom 并调用 ingest 写入。
2. **文章分类继承源分类** —— 已知问题：ingest 不传 `category` 时全部落 `other`。
   - 注意：`Category` 参与 `ContentHashV1` 计算，改动会影响去重判定，需先评估影响面
   - 目前界面已不暴露分类 Tab，用户可见影响已消除，但接口层 `?category=` 仍返回 0 条
3. **讯飞 TTS provider** —— 补齐第三个国内云厂商实现

### 中期

4. **Android 端**（Kotlin + Jetpack Compose）—— 按既定节奏后置，服务端 API 已为它预留（refresh token 支持 body 传输通道）
5. **多源筛选** —— 目前只支持单源（`sourceId`）。若要「同时看 IT之家 + 少数派」需新增 `sourceIds` 参数（服务端改动：`ArticleQuery` + `IN (...)` + openapi + 测试）
6. **源图标** —— 列表 DTO 未带 `sourceIconUrl`（SELECT 未取 `s.icon_url`）

### 待评估

7. **部署文档** —— Docker 之外的裸机部署说明、反向代理与 HTTPS 配置
8. **监控与可观测性** —— 当前有 `/health` 与基础日志，无指标暴露

## 五、已知约束与注意事项

- **服务端禁止出现抓取新闻源的 HTTP 客户端**（仅 TTS 出网）—— 这是架构核心约束，勿违反
- **无推送**：不要引入 WebSocket / 长连接 / 厂商推送
- **`docs/api/openapi.yaml` 是 CRLF 行尾** —— 用脚本按 `\n` 锚点改会静默全部失效
- **JWT 密钥环境变量带 `AUTH_` 前缀**：`EZNEWS_AUTH_JWT_SECRET`（写成 `EZNEWS_JWT_SECRET` 会被展开成空串，导致每次重启密钥重新生成、所有用户被集体登出）
- **`refreshCookiePath` 必须保持 `/api/v1/auth` 宽路径** —— 窄化会让登出形同虚设
