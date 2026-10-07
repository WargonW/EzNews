package util

import (
	"net/url"
	"sort"
	"strings"
)

// NormalizeURL 按 §3.4.2 Step 2 规范化 URL，返回规范化后的字符串。
//
// 步骤：trim → scheme/host 小写 → 去默认端口 → 去 fragment → 删除追踪参数 →
// query 按键升序重排 → path 去掉末尾 "/"（保留根 "/"）。
//
// blacklist 支持前缀通配（如 "utm_*"）与精确匹配（如 "spm"）。
func NormalizeURL(raw string, blacklist []string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ErrEmptyURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Host == "" {
		return "", ErrInvalidURL
	}
	// scheme / host 小写
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)

	// 去默认端口
	if (u.Scheme == "http" && strings.HasSuffix(u.Host, ":80")) ||
		(u.Scheme == "https" && strings.HasSuffix(u.Host, ":443")) {
		if idx := strings.LastIndex(u.Host, ":"); idx > 0 {
			u.Host = u.Host[:idx]
		}
	}

	// 去 fragment
	u.Fragment = ""

	// 过滤追踪参数后按键升序重排
	q := u.Query()
	for k := range q {
		if matchBlacklist(k, blacklist) {
			q.Del(k)
		}
	}
	if len(q) == 0 {
		u.RawQuery = ""
	} else {
		keys := make([]string, 0, len(q))
		for k := range q {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			vals := q[k]
			sort.Strings(vals)
			for _, v := range vals {
				parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v))
			}
		}
		u.RawQuery = strings.Join(parts, "&")
	}

	// path 去掉末尾 "/"（保留根 "/"）
	if len(u.Path) > 1 {
		u.Path = strings.TrimRight(u.Path, "/")
		if u.Path == "" {
			u.Path = "/"
		}
	}
	return u.String(), nil
}

// matchBlacklist 判断 query 参数名是否命中黑名单（支持 "prefix_*" 形式）。
func matchBlacklist(key string, blacklist []string) bool {
	lower := strings.ToLower(key)
	for _, rule := range blacklist {
		rule = strings.ToLower(strings.TrimSpace(rule))
		if rule == "" {
			continue
		}
		if strings.HasSuffix(rule, "*") {
			if strings.HasPrefix(lower, strings.TrimSuffix(rule, "*")) {
				return true
			}
			continue
		}
		if lower == rule {
			return true
		}
	}
	return false
}

// IsHTTPURL 判断 URL 是否为合法的 http/https 链接（ingest 必填校验用）。
func IsHTTPURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return u.Host != ""
}
