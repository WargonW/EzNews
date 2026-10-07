-- ==================== 003_fts：全文检索（P1，可选） ====================
-- 由 store 迁移器按 search.enableFts 决定是否执行；
-- trigram tokenizer 需要 SQLite >= 3.34，失败时迁移器会降级为 unicode61 或跳过。

CREATE VIRTUAL TABLE IF NOT EXISTS article_fts USING fts5(
  title,
  summary,
  content = 'article',
  content_rowid = 'id',
  tokenize = 'trigram'      -- 支持中文子串匹配
);

-- 同步触发器（外部内容表必须手动维护）
CREATE TRIGGER IF NOT EXISTS trg_article_ai AFTER INSERT ON article BEGIN
  INSERT INTO article_fts(rowid, title, summary) VALUES (new.id, new.title, new.summary);
END;
CREATE TRIGGER IF NOT EXISTS trg_article_ad AFTER DELETE ON article BEGIN
  INSERT INTO article_fts(article_fts, rowid, title, summary) VALUES('delete', old.id, old.title, old.summary);
END;
CREATE TRIGGER IF NOT EXISTS trg_article_au AFTER UPDATE ON article BEGIN
  INSERT INTO article_fts(article_fts, rowid, title, summary) VALUES('delete', old.id, old.title, old.summary);
  INSERT INTO article_fts(rowid, title, summary) VALUES (new.id, new.title, new.summary);
END;
