package observe

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics 是进程内指标计数器（P1，无 Prometheus 依赖）。
// 所有方法并发安全；Get 返回快照供 /metrics 输出。
type Metrics struct {
	articles       atomic.Int64
	ingested       atomic.Int64
	ingestDay      atomic.Value // string: YYYY-MM-DD（UTC）
	ingestedToday  atomic.Int64
	ttsSynthesized atomic.Int64
	ttsCacheHit    atomic.Int64
	ttsFailed      atomic.Int64
	audioBytes     atomic.Int64

	mu             sync.Mutex
	startAt        time.Time
	articlesLoaded bool
}

// NewMetrics 创建指标实例（可选传入 store 运行时）。
func NewMetrics() *Metrics {
	m := &Metrics{startAt: time.Now()}
	m.ingestDay.Store(time.Now().UTC().Format("2006-01-02"))
	return m
}

// StartAt 返回进程启动时间，用于计算 uptimeSec。
func (m *Metrics) StartAt() time.Time { return m.startAt }

// AddIngested 累加 ingest 接收条目数，并维护"今日"计数（跨 UTC 零点自动归零）。
func (m *Metrics) AddIngested(n int64) {
	m.ingested.Add(n)
	today := time.Now().UTC().Format("2006-01-02")
	if m.ingestDay.Load().(string) != today {
		m.mu.Lock()
		if m.ingestDay.Load().(string) != today {
			m.ingestDay.Store(today)
			m.ingestedToday.Store(0)
		}
		m.mu.Unlock()
	}
	m.ingestedToday.Add(n)
}

// AddTTS 记录一次合成结果。cacheHit 表示命中音频缓存。
func (m *Metrics) AddTTS(success, cacheHit bool, bytes int64) {
	if cacheHit {
		m.ttsCacheHit.Add(1)
		return
	}
	if success {
		m.ttsSynthesized.Add(1)
		m.audioBytes.Add(bytes)
		return
	}
	m.ttsFailed.Add(1)
}

// SetArticles 设置文章总数快照（由 /metrics 读取时惰性刷新）。
func (m *Metrics) SetArticles(n int64) { m.articles.Store(n) }

// Snapshot 返回 /metrics 所需的全部字段。
func (m *Metrics) Snapshot() map[string]any {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return map[string]any{
		"articles":       m.articles.Load(),
		"ingestedTotal":  m.ingested.Load(),
		"ingestedToday":  m.ingestedToday.Load(),
		"ttsSynthesized": m.ttsSynthesized.Load(),
		"ttsCacheHit":    m.ttsCacheHit.Load(),
		"ttsFailed":      m.ttsFailed.Load(),
		"audioBytes":     m.audioBytes.Load(),
		"goroutines":     runtime.NumGoroutine(),
		"heapAllocBytes": ms.HeapAlloc,
		"uptimeSec":      int64(time.Since(m.startAt).Seconds()),
	}
}
