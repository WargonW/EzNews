package service

import (
	"sync"
	"time"
)

// 吊销会话缓存的容量与存活期。
//
// 取值依据（改动前请先读这段，避免"随手调大"变成内存泄漏）：
//
//   - maxRevokedSessionEntries = 16384
//     单实例自托管部署，活跃会话数远低于此值。每条约 8B key + 24B time + map 开销，
//     合计约 50~60B，16384 条的内存上界约 1 MB。选 16384 是"比现实负载高约两个数量级、
//     但内存硬上界只有 ~1MB"的平衡点：既能吸收单用户多设备 + 短时集中吊销的突发，
//     又不会让一个被恶意批量吊销的调用方把进程内存撑大。
//
//   - revokedSessionEntryTTL = 30 * time.Minute
//     本缓存是**纯性能层，DB 才是权威**：条目过期后下次请求回源 session 表，
//     依然会判为"已吊销"→ 401。因此 TTL 缩短只影响"被吊销 token 被高频重放时的回源频率"，
//     不影响正确性。取 30 分钟意味着即使有人拿已吊销的 token 狂刷，也最多 30 分钟回源一次，
//     而正常用户的活跃会话根本不在本缓存里（这里只存已吊销态），不受任何影响。
//
// 为什么**只缓存"已吊销"、不缓存"活跃"**（这是本设计的核心取舍，改动前务必理解）：
//
//	若缓存"活跃"结论，被吊销的会话会在 TTL 内因缓存命中而继续通过校验 → 违反
//	"单会话吊销后 access token 必须立即 401"这一硬要求。
//	只缓存吊销态则：吊销时写穿透 → 后续请求零 DB 查询直接 401；
//	未吊销会话永远不命中 → 每请求 1 次主键索引查询回源。
//	这正好落在"已登录请求最多增加一次轻量主键查询"的预算内，且正确性永不依赖缓存。
const (
	maxRevokedSessionEntries = 16384
	revokedSessionEntryTTL   = 30 * time.Minute
)

// revokedSessionCache 是"已吊销会话"的进程内缓存：sessionID -> 吊销时刻。
//
// 用 RWMutex + map 而非 sync.Map：本缓存必须能强制容量上界并支持惰性清扫，
// sync.Map 做不到按 TTL 批量剔除，接手的人容易写出无界增长的版本。
//
// 零值不可用，请使用 newRevokedSessionCache 构造。
type revokedSessionCache struct {
	mu      sync.RWMutex
	entries map[int64]time.Time
}

// newRevokedSessionCache 创建吊销会话缓存。
func newRevokedSessionCache() *revokedSessionCache {
	return &revokedSessionCache{entries: make(map[int64]time.Time, 64)}
}

// Lookup 查询某会话是否已被吊销。
//
// hit=false 表示缓存中没有结论（调用方需回源 DB），此时 revoked 的取值无意义。
func (c *revokedSessionCache) Lookup(sessionID int64) (revoked bool, hit bool) {
	if c == nil {
		return false, false
	}
	c.mu.RLock()
	revokedAt, ok := c.entries[sessionID]
	c.mu.RUnlock()
	if !ok {
		return false, false
	}
	// 条目已过期：视为无结论，交由调用方回源 DB（DB 才是权威）。
	// 这里不返回 true，避免时钟漂移或清扫延迟把一个未被吊销的会话误判为已吊销。
	if time.Since(revokedAt) > revokedSessionEntryTTL {
		return false, false
	}
	return true, true
}

// Put 记录某会话已被吊销。
func (c *revokedSessionCache) Put(sessionID int64, revokedAt time.Time) {
	if c == nil || sessionID <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	// 先清扫过期条目再判断是否超限：吊销是低频事件，这里做一次全量遍历的代价可忽略，
	// 换来的是绝大多数情况下无需额外的淘汰逻辑。
	c.evictExpiredLocked(revokedAt)

	// 仍超上限时淘汰任意条目。必须保证不超上限——被淘汰只会让下次请求回源 DB，
	// 不会导致已吊销的 token 被放行，所以这里可以放心地牺牲缓存命中率换内存上界。
	for len(c.entries) >= maxRevokedSessionEntries {
		for id := range c.entries {
			delete(c.entries, id)
			break
		}
	}
	c.entries[sessionID] = revokedAt
}

// evictExpiredLocked 惰性清除过期条目，调用方必须已持有写锁。
func (c *revokedSessionCache) evictExpiredLocked(now time.Time) {
	for id, at := range c.entries {
		if now.Sub(at) > revokedSessionEntryTTL {
			delete(c.entries, id)
		}
	}
}

// Len 返回当前缓存条目数（测试与观测用）。
func (c *revokedSessionCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}
