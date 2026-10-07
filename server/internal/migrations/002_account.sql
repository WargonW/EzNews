-- ==================== 002_account：账号体系与数据同步（v1.1） ====================

-- ============ 7. session（Refresh Token） ============
-- ★ 行身份不变量：一次逻辑会话 = 一行，轮换时必须原地 UPDATE。
--   两处依赖它：DEC-7 的 60s 宽限判定（prev_* 记录本行上一代 hash）、
--   DEC-14 的 sid 稳定性（JWT 里的 sid = session.id）。若改成 delete + insert，
--   两个修复会同时静默失效：宽限判定无处落 prev 列，sid 每次刷新都变导致登出失效。
CREATE TABLE IF NOT EXISTS session (
  id                      INTEGER PRIMARY KEY AUTOINCREMENT,  -- AUTOINCREMENT：rowid 不复用，
                                                                        -- 防止老 token 的 sid 命中他人会话
  user_id                 INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
  refresh_token_hash      TEXT    NOT NULL,        -- hex(sha256(token))，绝不明文
  prev_refresh_token_hash TEXT,                    -- 上一代 hash（轮换时移入，不置空）
  prev_rotated_at         INTEGER,                 -- 上一代被轮换的时刻，宽限窗口起算点
  device_name             TEXT,
  client_id               TEXT    NOT NULL,
  last_seen_at            INTEGER NOT NULL,
  expires_at              INTEGER NOT NULL,        -- 30 天
  revoked_at              INTEGER,                 -- 非 0 = 已作废（reuse 检测 + 踢下线）
  created_at              INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_session_rt    ON session(refresh_token_hash);
CREATE INDEX        IF NOT EXISTS idx_session_user  ON session(user_id, expires_at);
CREATE INDEX        IF NOT EXISTS idx_session_seen  ON session(user_id, last_seen_at DESC);
-- 部分索引：只覆盖存在上一代 hash 的少数行，几乎不占空间
CREATE INDEX        IF NOT EXISTS idx_session_prev  ON session(prev_refresh_token_hash)
  WHERE prev_refresh_token_hash IS NOT NULL;

-- ============ 8. user_favorite（收藏，含墓碑） ============
CREATE TABLE IF NOT EXISTS user_favorite (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id     INTEGER NOT NULL REFERENCES user(id)    ON DELETE CASCADE,
  article_id  INTEGER NOT NULL REFERENCES article(id) ON DELETE CASCADE,
  deleted_at  INTEGER,                 -- 墓碑：非 NULL = 已取消收藏
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL         -- LWW 冲突判定依据
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_fav      ON user_favorite(user_id, article_id);  -- ★ 幂等防翻倍
CREATE INDEX        IF NOT EXISTS idx_fav_sync ON user_favorite(user_id, updated_at, id);

-- ============ 9. user_read（已读，含墓碑） ============
CREATE TABLE IF NOT EXISTS user_read (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id     INTEGER NOT NULL REFERENCES user(id)    ON DELETE CASCADE,
  article_id  INTEGER NOT NULL REFERENCES article(id) ON DELETE CASCADE,
  deleted_at  INTEGER,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_read       ON user_read(user_id, article_id);
CREATE INDEX        IF NOT EXISTS idx_read_sync ON user_read(user_id, updated_at, id);

-- ============ 10. user_preference（偏好 KV） ============
CREATE TABLE IF NOT EXISTS user_preference (
  user_id     INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
  key         TEXT    NOT NULL,        -- tts.voice / tts.speed / ui.density …
  value       TEXT    NOT NULL,        -- JSON 标量或字符串
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY (user_id, key)
) WITHOUT ROWID;

-- ============ 11. merge_log（合并幂等日志） ============
CREATE TABLE IF NOT EXISTS merge_log (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id     INTEGER NOT NULL REFERENCES user(id) ON DELETE CASCADE,
  client_id   TEXT    NOT NULL,
  nonce       TEXT    NOT NULL,
  result_json TEXT    NOT NULL,        -- 结果快照；重复提交直接回放
  created_at  INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_merge ON merge_log(user_id, client_id, nonce);  -- ★ 幂等落地
