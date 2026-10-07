package auth

import (
	"crypto/rand"
	"fmt"
	"io"
)

// randomFill 用 crypto/rand 填满缓冲区。
func randomFill(b []byte) error {
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return fmt.Errorf("生成随机数失败: %w", err)
	}
	return nil
}
