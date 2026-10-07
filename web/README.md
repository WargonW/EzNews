# EZNews Web 端

游客可用 + 可选登录的新闻阅读器与源管理工作台。

## 快速开始

```bash
pnpm install
pnpm dev      # http://localhost:5173
```

开发期不需要配置任何环境变量：`vite.config.ts` 已把
`/api`、`/healthz`、`/readyz`、`/metrics` 代理到 `http://127.0.0.1:8080`，
浏览器侧一律使用同源相对路径 `/api/v1/...`（这样 httpOnly refresh Cookie 才能带上）。

| 命令 | 说明 |
| --- | --- |
| `pnpm dev` | 开发服务器（端口 5173，`strictPort`，被占用直接报错不换端口） |
| `pnpm build` | `tsc -b` 类型检查 + 生产构建，产物在 `dist/` |
| `pnpm preview` | 预览构建产物 |
| `pnpm typecheck` | `tsc --noEmit` 零错误校验 |
| `pnpm lint` | ESLint（`--max-warnings 0`，零容忍） |

## 目录结构

```
web/
├── index.html                 首屏占位骨架（纯 CSS，bundle 加载期间防白屏）
├── vite.config.ts             dev proxy / 路由级 chunk 分离
├── tailwind.config.js         设计令牌 → Tailwind 语义色（darkMode: 'media'）
├── pnpm-workspace.yaml        pnpm 设置（构建脚本放行 + hoisted 布局）
├── .env.example               VITE_API_BASE_URL 说明（默认留空 = 同源）
└── src/
    ├── main.tsx               入口：挂载 React + 移除 #boot 占位
    ├── App.tsx                外壳：QueryClient / 路由 / 主题 / 错误边界
    ├── api/
    │   ├── types.ts           ★ openapi.yaml 手工转写的全部类型（无 any）
    │   ├── client.ts          ★ 鉴权、单飞 refresh、错误归一化
    │   └── endpoints.ts       一函数 = 一个 operationId
    ├── lib/
    │   ├── sync.ts            ★ 增量游标（基线/推进/合并）
    │   ├── storage.ts         localStorage 安全封装（含凭证泄漏护栏）
    │   ├── queryKeys.ts       queryKey 工厂（防拼写漂移）
    │   ├── datetime.ts        ISO8601 → 本地时区展示
    │   └── constants.ts       分类/源类型的展示映射
    ├── stores/
    │   ├── auth.ts            Access Token 仅内存
    │   ├── guest.ts           ★ 游客态收藏/已读（含墓碑）+ 偏好
    │   └── player.ts          播放队列
    ├── hooks/
    │   ├── useSessionBootstrap.ts   启动静默恢复 + 回前台增量
    │   ├── useMergeGuestData.ts     ★ Merge Policy v1 + nonce 幂等
    │   ├── useUserStateSync.ts      云端收藏/已读/偏好增量同步
    │   ├── useArticleFeed.ts        列表：分页 + 增量合并 + 本地态覆盖
    │   ├── useFavoriteActions.ts    先本地后云端，失败不回滚
    │   └── useArticleAudio.ts       服务端 TTS 状态机 + 退避轮询
    ├── components/            TopBar / Sidebar / ArticleCard / AuthDialog / ...
    └── pages/                 FeedPage / ArticleDetailPage / FavoritesPage / SettingsPage
```

## 路由表

| 路径 | 组件 | 加载方式 | 说明 |
| --- | --- | --- | --- |
| `/` | `FeedPage` | 静态导入（首屏） | 文章流 + 侧栏筛选；筛选态同步到 URL |
| `/article/:id` | `ArticleDetailPage` | `React.lazy` | 详情 + 自动标记已读 + 语音播报 |
| `/favorites` | `FavoritesPage` | `React.lazy` | 我的收藏 |
| `/settings` | `SettingsPage` | `React.lazy` | 偏好 + 账号 + 会话管理 |
| `*` | `NotFoundPage` | `React.lazy` | 404 |

## 关键设计决策

### 1. 增量游标（省流量的核心）

- 双模式由服务端区分：**不传 `cursor`** = 首屏/翻页（`published_at DESC`，返回
  `pageCursor` + `syncCursor`）；**传 `cursor`** = 增量（`updated_at ASC`，用 `nextCursor`）。
- ★ **基线只能来自首屏响应的 `syncCursor`**。增量响应里的 `nextCursor` 语义是
  "这次读到哪了"，不能当新基线，否则会漏数据。见 `src/hooks/useArticleFeed.ts:82-90`。
- ★ **中途达到 `maxPages` 上限不推进游标** —— 宁可多拉，绝不漏拉。见 `src/lib/sync.ts`。
- 增量请求**默认不传筛选条件**：带筛选会漏掉"移出筛选集"的变化。
- 触发时机：手动「增量刷新」按钮 + 回到前台（`visibilitychange`）+ 网络恢复（`online`），
  冷却窗口 15s，且关闭了 TanStack 的 `refetchOnWindowFocus`，避免双通道重复请求。

### 2. 单飞 refresh（DEC-10）

`src/api/client.ts:109-172`。模块级 `refreshInFlight` 保存进行中的刷新 Promise：

- 首个遇到 `401 + TOKEN_EXPIRED` 的请求创建 Promise 并发起刷新；
- 其余并发请求 await **同一个** Promise，拿到同一份新 token 后各自重放；
- 刷新失败统一 reject 并降级游客态，不各自重试；
- `_isRetry` 标记保证同一请求**最多重放一次**，避免形成死循环；
- refresh 返回 401 时置 30s 退避窗口，防止凭证已死时打满服务端限流。

为什么必须这样：Access Token 有效期 2h，多个请求同时过期是常态。若各自发一次
`/auth/refresh`，服务端会看到同一 refresh token 的并发轮换 —— 第二发带的是已作废的
旧串，命中 reuse 检测，可能触发**撤销该用户全部 session + token_version+1**，
用户会被自己的并发请求踢下线。

### 3. 会话静默恢复

Access Token 只存内存，刷新页面后必然为空。此时发业务请求不带 `Authorization`，
服务端返回的是 `401 UNAUTHORIZED`（而非 `TOKEN_EXPIRED`），按 client 策略**不会触发刷新**
—— 会话永远恢复不了。所以 `client.ts` 单独导出 `restoreSession()`，
在 `useSessionBootstrap` 里启动时显式调一次（内部复用同一个 single-flight）。

### 4. 合并策略（Merge Policy v1）

`src/hooks/useMergeGuestData.ts` + `src/stores/guest.ts`：

- 收藏/已读：并集 + LWW(`updatedAt`) + **墓碑**（`deleted_at`）。
  本地用 `Map<articleId, {updatedAt, deleted}>` 而非 `Set`，
  否则无法区分"从未收藏"与"明确取消"，后者会被云端旧记录复活。
- 偏好：整包 KV，**服务端有值以服务端为准**。合并成功后**绝不**调 `putPreferences`
  把本地偏好写回去（那会把服务端多设备设置反向冲掉）。
- nonce 持久化到成功为止，保证网络超时重试时服务端不会应用两遍。
- 合并成功后 `resetGuestState()` 清空游客态。
- ★ **客户端时间戳钳制**：`updatedAt <= 0` 或超前 > 7 天一律压到"此刻"
  （`clampClientTimestamp`）。否则客户端时钟不准会让本地数据**永久赢过服务端**，
  之后服务端怎么改都同步不下去，且重装无法收敛。

### 5. 登录入口只有两处

顶栏与设置面板。任何浏览路径（筛选/搜索/播放/收藏/取消收藏）都不会弹登录窗或注册墙。
游客点收藏立即写本地并给一条**可关闭的轻提示**（PRD R3 明确允许），不做任何阻断。

### 6. 深浅色主题

CSS 变量 + Tailwind `<alpha-value>`：`rgb(var(--ez-surface) / <alpha-value>)`，
深浅色只换变量值，不需要满屏 `dark:` 变体。默认跟随 `prefers-color-scheme`，
顶栏可手动切换 system → light → dark 循环。

## 已知契约限制

服务端未提供"按收藏取文章"的接口（`GET /me/favorites` 只返回 `articleId`），
所以"我的收藏"页只能显示已加载过的文章，并提供「从服务端重新拉取」按钮尽力补齐。
详见 `src/pages/FavoritesPage.tsx` 文件头注释与交付报告。