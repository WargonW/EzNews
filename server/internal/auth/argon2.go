package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id 默认参数（PRD §7.2 / ARCHITECTURE §2.5）。
const (
	// DefaultArgon2Memory 默认内存开销（KiB）= 19 MiB。
	DefaultArgon2Memory uint32 = 19456
	// DefaultArgon2Time 默认迭代次数。
	DefaultArgon2Time uint32 = 2
	// DefaultArgon2Threads 默认并行度。
	DefaultArgon2Threads uint8 = 1
)

// ErrInvalidPHC 表示 PHC 字符串格式非法。
var ErrInvalidPHC = errors.New("密码哈希(PHC)格式非法")

// Argon2Hasher 封装 Argon2id 的哈希与校验（参数随 PHC 字符串存储，存量密码不受调参影响）。
type Argon2Hasher struct {
	Memory  uint32
	Time    uint32
	Threads uint8
	KeyLen  uint32
	SaltLen int
}

// NewArgon2Hasher 创建哈希器，零值参数回填为默认值。
func NewArgon2Hasher(memory, time uint32, threads uint8) *Argon2Hasher {
	if memory == 0 {
		memory = DefaultArgon2Memory
	}
	if time == 0 {
		time = DefaultArgon2Time
	}
	if threads == 0 {
		threads = DefaultArgon2Threads
	}
	return &Argon2Hasher{Memory: memory, Time: time, Threads: threads, KeyLen: 32, SaltLen: 16}
}

// Hash 生成 Argon2id PHC 字符串：$argon2id$v=19$m=..,t=..,p=..$<salt>$<hash>
func (h *Argon2Hasher) Hash(password string) (string, error) {
	salt := make([]byte, h.SaltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", fmt.Errorf("生成盐失败: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, h.Time, h.Memory, h.Threads, h.KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, h.Memory, h.Time, h.Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// Verify 校验密码与 PHC 字符串是否匹配（按 PHC 内参数执行，常数时间比较）。
func (h *Argon2Hasher) Verify(password, phc string) (bool, error) {
	params, salt, key, err := parsePHC(phc)
	if err != nil {
		return false, err
	}
	candidate := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.threads, uint32(len(key)))
	return subtle.ConstantTimeCompare(candidate, key) == 1, nil
}

type argon2Params struct {
	memory  uint32
	time    uint32
	threads uint8
}

// parsePHC 解析 Argon2id PHC 字符串。
func parsePHC(phc string) (argon2Params, []byte, []byte, error) {
	var empty argon2Params
	parts := strings.Split(strings.TrimSpace(phc), "$")
	// parts[0] 为空（前导 $），parts[1]="argon2id"，parts[2]="v=19"，parts[3]=参数，parts[4]=salt，parts[5]=hash
	if len(parts) != 6 || parts[1] != "argon2id" {
		return empty, nil, nil, ErrInvalidPHC
	}
	var p argon2Params
	for _, item := range strings.Split(parts[3], ",") {
		kv := strings.SplitN(strings.TrimSpace(item), "=", 2)
		if len(kv) != 2 {
			return empty, nil, nil, ErrInvalidPHC
		}
		v, err := strconv.ParseUint(kv[1], 10, 32)
		if err != nil {
			return empty, nil, nil, ErrInvalidPHC
		}
		switch kv[0] {
		case "m":
			p.memory = uint32(v)
		case "t":
			p.time = uint32(v)
		case "p":
			if v == 0 || v > 255 {
				return empty, nil, nil, ErrInvalidPHC
			}
			p.threads = uint8(v)
		}
	}
	if p.memory == 0 || p.time == 0 || p.threads == 0 {
		return empty, nil, nil, ErrInvalidPHC
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return empty, nil, nil, ErrInvalidPHC
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return empty, nil, nil, ErrInvalidPHC
	}
	return p, salt, key, nil
}
