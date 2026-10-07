-- ==================== 004_seed：系统默认源种子数据（幂等） ====================
-- 使用 INSERT OR IGNORE + ux_source_key 唯一索引保证重复执行不产生重复行。
-- 采集器通过 GET /api/v1/sources 拉取这些源（key + suggestInterval）。

INSERT OR IGNORE INTO source
  (key, name, url, type, category, is_default, enabled, suggest_interval, language, created_at, updated_at)
VALUES
  ('sspai',       '少数派',     'https://sspai.com',                'rss', 'tech',     1, 1, 1800, 'zh-CN', (strftime('%s','now')*1000), (strftime('%s','now')*1000)),
  ('ithome',      'IT之家',     'https://www.ithome.com',           'rss', 'tech',     1, 1, 900,  'zh-CN', (strftime('%s','now')*1000), (strftime('%s','now')*1000)),
  ('cnbeta',      'cnBeta.COM', 'https://www.cnbeta.com.tw',        'rss', 'tech',     1, 1, 1800, 'zh-CN', (strftime('%s','now')*1000), (strftime('%s','now')*1000)),
  ('solidot',     'Solidot',    'https://www.solidot.org',          'rss', 'tech',     1, 1, 3600, 'zh-CN', (strftime('%s','now')*1000), (strftime('%s','now')*1000)),
  ('36kr',        '36氪',       'https://36kr.com',                 'rss', 'finance',  1, 1, 1800, 'zh-CN', (strftime('%s','now')*1000), (strftime('%s','now')*1000)),
  ('huxiu',       '虎嗅',       'https://www.huxiu.com',            'rss', 'finance',  1, 1, 1800, 'zh-CN', (strftime('%s','now')*1000), (strftime('%s','now')*1000)),
  ('thepaper',    '澎湃新闻',   'https://www.thepaper.cn',          'rss', 'china',    1, 1, 1800, 'zh-CN', (strftime('%s','now')*1000), (strftime('%s','now')*1000)),
  ('ifeng',       '凤凰网',     'https://www.ifeng.com',            'rss', 'china',    1, 1, 1800, 'zh-CN', (strftime('%s','now')*1000), (strftime('%s','now')*1000)),
  ('zaobao',      '联合早报',   'https://www.zaobao.com',           'rss', 'world',    1, 1, 3600, 'zh-CN', (strftime('%s','now')*1000), (strftime('%s','now')*1000)),
  ('dongqiudi',   '懂球帝',     'https://www.dongqiudi.com',        'rss', 'sports',   1, 1, 1800, 'zh-CN', (strftime('%s','now')*1000), (strftime('%s','now')*1000)),
  ('guokr',       '果壳',       'https://www.guokr.com',            'rss', 'science',  1, 1, 3600, 'zh-CN', (strftime('%s','now')*1000), (strftime('%s','now')*1000)),
  ('dxy',         '丁香园',     'https://www.dxy.cn',               'rss', 'health',   1, 1, 3600, 'zh-CN', (strftime('%s','now')*1000), (strftime('%s','now')*1000)),
  ('mtime',       '时光网',     'https://www.mtime.com',            'rss', 'ent',      1, 1, 3600, 'zh-CN', (strftime('%s','now')*1000), (strftime('%s','now')*1000)),
  ('autohome',    '汽车之家',   'https://www.autohome.com.cn',      'rss', 'auto',     1, 1, 3600, 'zh-CN', (strftime('%s','now')*1000), (strftime('%s','now')*1000)),
  ('sinamil',     '新浪军事',   'https://mil.news.sina.com.cn',     'rss', 'military', 1, 1, 3600, 'zh-CN', (strftime('%s','now')*1000), (strftime('%s','now')*1000)),
  ('lifeweek',    '三联生活周刊','https://www.lifeweek.com.cn',     'rss', 'life',     1, 1, 3600, 'zh-CN', (strftime('%s','now')*1000), (strftime('%s','now')*1000));
