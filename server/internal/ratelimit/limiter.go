// Package ratelimit 提供进程内的全局 + 分键令牌桶限流（ARCHITECTURE.md §3.7）。
//
// 设计取舍：单实例自托管场景，无需 Redis 等外部依赖；多实例部署时在入口网关做限流。
// 分键桶采用 LRU 淘汰，避免长期运行导致内存无界增长。
package ratelimit

import (
	"container/list"
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// bucket 是一个分键令牌桶及其在 LRU 链表中的位置。
type bucket struct {
	key    string
	lim    *rate.Limiter
	seenAt time.Time
	elem   *list.Element
}

// Limiter 同时持有全局桶与按 key 划分的桶集合。
//
// 零值不可用，请使用 New 构造。
type Limiter struct {
	global *rate.Limiter

	mu      sync.Mutex
	entries map[string]*bucket
	lru     *list.List // front = 最近使用
	maxKeys int

	rps   rate.Limit
	burst int
	ttl   time.Duration
}

// New 创建限流器。
//
// rps 为每秒补充速率（<=0 表示不限流），burst 为桶容量，maxKeys 为分键桶上限，ttl 为分键桶空闲淘汰时间。
func New(rps float64, burst int, maxKeys int, ttl time.Duration) *Limiter {
	if rps <= 0 {
		rps = 0
	}
	if burst <= 0 {
		burst = 1
	}
	if maxKeys <= 0 {
		maxKeys = 1024
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	var g *rate.Limiter
	if rps > 0 {
		g = rate.NewLimiter(rate.Limit(rps), burst)
	}
	return &Limiter{
		global:  g,
		entries: make(map[string]*bucket, 64),
		lru:     list.New(),
		maxKeys: maxKeys,
		rps:     rate.Limit(rps),
		burst:   burst,
		ttl:     ttl,
	}
}

// Allow 判断一次请求是否放行；key 为空时只走全局桶。
func (l *Limiter) Allow(key string) bool {
	return l.AllowN(key, 1)
}

// AllowN 判断 n 个单位的请求是否放行。
func (l *Limiter) AllowN(key string, n int) bool {
	if l == nil {
		return true
	}
	now := time.Now()
	if l.global != nil && !l.global.AllowN(now, n) {
		return false
	}
	if key == "" || l.rps <= 0 {
		return true
	}
	return l.bucketFor(key, now).lim.AllowN(now, n)
}

// Wait 阻塞等待令牌，受 ctx 约束（用于需要背压而非直接拒绝的场景）。
func (l *Limiter) Wait(ctx context.Context, key string) error {
	if l == nil {
		return nil
	}
	now := time.Now()
	if l.global != nil {
		if err := l.global.WaitN(ctx, 1); err != nil {
			return err
		}
	}
	if key == "" || l.rps <= 0 {
		return nil
	}
	return l.bucketFor(key, now).lim.Wait(ctx)
}

// Reset 清空指定 key 的桶（登录成功等场景用于解除惩罚）。
func (l *Limiter) Reset(key string) {
	if l == nil || key == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.entries[key]; ok {
		l.lru.Remove(b.elem)
		delete(l.entries, key)
	}
}

// bucketFor 取出（或新建）key 对应的桶，并维护 LRU 与空闲淘汰。
func (l *Limiter) bucketFor(key string, now time.Time) *bucket {
	l.mu.Lock()
	defer l.mu.Unlock()

	if b, ok := l.entries[key]; ok {
		b.seenAt = now
		l.lru.MoveToFront(b.elem)
		return b
	}

	// 淘汰：先清理过期项，仍超限则淘汰最久未使用项
	if l.ttl > 0 {
		for e := l.lru.Back(); e != nil; {
			b := e.Value.(*bucket)
			if now.Sub(b.seenAt) <= l.ttl {
				break
			}
			prev := e.Prev()
			l.lru.Remove(e)
			delete(l.entries, keyOfBucket(b))
			e = prev
		}
	}
	for l.lru.Len() >= l.maxKeys {
		e := l.lru.Back()
		if e == nil {
			break
		}
		b := e.Value.(*bucket)
		l.lru.Remove(e)
		delete(l.entries, keyOfBucket(b))
	}

	b := &bucket{
		key:    key,
		lim:    rate.NewLimiter(l.rps, l.burst),
		seenAt: now,
	}
	b.elem = l.lru.PushFront(b)
	l.entries[key] = b
	return b
}

// keyOfBucket 返回桶对应的 key，供淘汰时从 map 中删除。
func keyOfBucket(target *bucket) string {
	return target.key
}

// Len 返回当前分键桶数量（测试与观测用）。
func (l *Limiter) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}
