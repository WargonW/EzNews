-- ==================== 001_init：核心表 ====================
-- 注意：user 表必须在初始迁移中创建，因为 source.owner_user_id 需要外键引用它，
-- 而 SQLite 无法为已存在的表追加外键约束（ARCHITECTURE.md §4.3）。

-- ============ 1. 新闻源 ============
CREATE TABLE IF NOT EXISTS source (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  key              TEXT    NOT NULL,                -- 稳定业务键，采集器引用
  name             TEXT    NOT NULL,
  url              TEXT    NOT NULL,
  type             TEXT    NOT NULL DEFAULT 'rss',  -- rss | atom | api | manual
  category         TEXT    NOT NULL DEFAULT 'other',
  is_default       INTEGER NOT NULL DEFAULT 0,      -- 1=系统默认源（不可删除）
  enabled          INTEGER NOT NULL DEFAULT 1,      -- 停用后其文章不进入默认列表
  suggest_interval INTEGER NOT NULL DEFAULT 1800,   -- 建议采集间隔(秒)
  icon_url         TEXT,
  language         TEXT    NOT NULL DEFAULT 'zh-CN',
  remark           TEXT,
  owner_user_id    INTEGER REFERENCES user(id) ON DELETE CASCADE,  -- v1.1 预留：NULL=全局共享
  created_at       INTEGER NOT NULL,                -- epoch ms
  updated_at       INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_source_key    ON source(key);
CREATE INDEX        IF NOT EXISTS idx_source_list  ON source(enabled, category, name);
CREATE INDEX        IF NOT EXISTS idx_source_owner ON source(owner_user_id);

-- ============ 2. 文章 ============
CREATE TABLE IF NOT EXISTS article (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  source_id     INTEGER NOT NULL REFERENCES source(id) ON DELETE CASCADE,

  -- 去重键 1：source_id + external_id（external_id 可为空，用部分唯一索引）
  external_id   TEXT,
  -- 去重键 2：规范化 URL 的哈希
  url           TEXT    NOT NULL,   -- 原始 URL（展示/跳转用）
  url_norm      TEXT    NOT NULL,   -- 规范化 URL（排错用）
  url_hash      TEXT    NOT NULL,   -- hex(sha256(url_norm))[:32]

  title         TEXT    NOT NULL,
  summary       TEXT    NOT NULL DEFAULT '',
  content       TEXT,                        -- 默认不落库（ingest.storeContent=false）
  content_hash  TEXT    NOT NULL,            -- "v1:"+hex(sha256(canonical))[:32]

  category      TEXT    NOT NULL DEFAULT 'other',
  author        TEXT,
  image_url     TEXT,
  language      TEXT    NOT NULL DEFAULT 'zh-CN',
  tags_json     TEXT    NOT NULL DEFAULT '[]',   -- JSON 数组；不建关联表（省资源）

  published_at  INTEGER NOT NULL,   -- 发布时间（采集器提供）
  created_at    INTEGER NOT NULL,   -- 入库时间
  updated_at    INTEGER NOT NULL,   -- ★ 增量游标依据：仅内容真实变更时刷新
  last_seen_at  INTEGER NOT NULL    -- 每次被提交时刷新；★ 不参与游标
);

CREATE UNIQUE INDEX IF NOT EXISTS ux_article_ext
  ON article(source_id, external_id)
  WHERE external_id IS NOT NULL AND external_id <> '';   -- 部分唯一索引
CREATE UNIQUE INDEX IF NOT EXISTS ux_article_url  ON article(url_hash);

CREATE INDEX IF NOT EXISTS idx_article_cursor   ON article(updated_at, id);              -- 增量游标
CREATE INDEX IF NOT EXISTS idx_article_pub      ON article(published_at DESC, id DESC);  -- 列表排序
CREATE INDEX IF NOT EXISTS idx_article_src_pub  ON article(source_id, published_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_article_cat_pub  ON article(category, published_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_article_seen     ON article(last_seen_at);

-- ============ 3. TTS 合成任务 ============
CREATE TABLE IF NOT EXISTS audio_task (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  article_id    INTEGER NOT NULL REFERENCES article(id) ON DELETE CASCADE,
  voice         TEXT    NOT NULL,
  speed         REAL    NOT NULL,
  status        TEXT    NOT NULL DEFAULT 'pending',   -- pending|processing|ready|failed
  provider      TEXT,                                  -- tencent | ali | xunfei | mock
  audio_id      INTEGER,                               -- ready 后回填
  error_code    TEXT,
  error_msg     TEXT,
  retry_count   INTEGER NOT NULL DEFAULT 0,
  next_retry_at INTEGER,                               -- 退避重投时间
  text_chars    INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_task_cache  ON audio_task(article_id, voice, speed); -- 防重复合成
CREATE INDEX        IF NOT EXISTS idx_task_queue ON audio_task(status, next_retry_at, id);

-- ============ 4. 音频 ============
CREATE TABLE IF NOT EXISTS audio (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id        INTEGER REFERENCES audio_task(id) ON DELETE SET NULL,
  article_id     INTEGER NOT NULL REFERENCES article(id) ON DELETE CASCADE,
  voice          TEXT    NOT NULL,
  speed          REAL    NOT NULL,
  format         TEXT    NOT NULL DEFAULT 'mp3',
  file_path      TEXT    NOT NULL,      -- 相对 audioDir 的路径
  size_bytes     INTEGER NOT NULL DEFAULT 0,
  duration_ms    INTEGER NOT NULL DEFAULT 0,
  sample_rate    INTEGER NOT NULL DEFAULT 16000,
  provider       TEXT,
  hit_count      INTEGER NOT NULL DEFAULT 0,
  last_access_at INTEGER NOT NULL,      -- LRU 依据
  created_at     INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_audio_cache ON audio(article_id, voice, speed);
CREATE INDEX        IF NOT EXISTS idx_audio_lru   ON audio(last_access_at, size_bytes);

-- ============ 5. 用户（v1.1，需先于 source 的外键引用创建） ============
CREATE TABLE IF NOT EXISTS user (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  username       TEXT    NOT NULL,                 -- 3–32 字符，大小写敏感
  password_hash  TEXT    NOT NULL,                 -- Argon2id PHC 字符串
  email          TEXT,                             -- 选填，本期不验证
  role           TEXT    NOT NULL DEFAULT 'user',  -- admin 预留
  token_version  INTEGER NOT NULL DEFAULT 1,       -- +1 使全部已签发 Access Token 失效
  created_at     INTEGER NOT NULL,
  updated_at     INTEGER NOT NULL,
  last_login_at  INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_user_name  ON user(username);
CREATE INDEX        IF NOT EXISTS idx_user_email ON user(email) WHERE email IS NOT NULL;

-- ============ 6. 元数据 ============
CREATE TABLE IF NOT EXISTS schema_meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
INSERT OR IGNORE INTO schema_meta(key, value) VALUES ('schema_version', '0');
