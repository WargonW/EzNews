package auth

import (
	"context"
	"errors"
	"sync/atomic"
)

// ErrSemaphoreBusy 表示等待队列已满（并发 Argon2id 计算过多）。
var ErrSemaphoreBusy = errors.New("系统繁忙，请稍后重试")

// Semaphore 是带等待队列上限的计数信号量。
//
// 用途：限制并发 Argon2id 计算数量（默认 2），防止 19 MiB × N 的内存叠加
// 突破 150 MB 常驻内存目标（ARCHITECTURE.md §2.5）。
type Semaphore struct {
	ch         chan struct{}
	waiting    atomic.Int64
	queueLimit int64
}

// NewSemaphore 创建容量为 size、等待队列上限为 queueLimit 的信号量。
func NewSemaphore(size int, queueLimit int) *Semaphore {
	if size <= 0 {
		size = 1
	}
	if queueLimit <= 0 {
		queueLimit = 32
	}
	s := &Semaphore{ch: make(chan struct{}, size), queueLimit: int64(queueLimit)}
	return s
}

// Acquire 获取一个许可；等待队列满时返回 ErrSemaphoreBusy。
func (s *Semaphore) Acquire(ctx context.Context) error {
	if s.waiting.Load() >= s.queueLimit {
		return ErrSemaphoreBusy
	}
	s.waiting.Add(1)
	defer s.waiting.Add(-1)
	select {
	case s.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release 归还一个许可。
func (s *Semaphore) Release() {
	select {
	case <-s.ch:
	default:
	}
}
