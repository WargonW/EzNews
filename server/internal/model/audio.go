package model

// AudioTaskStatus 是合成任务状态。
type AudioTaskStatus string

// 任务状态枚举（状态机见 ARCHITECTURE.md §6.1）。
const (
	TaskStatusPending    AudioTaskStatus = "pending"
	TaskStatusProcessing AudioTaskStatus = "processing"
	TaskStatusReady      AudioTaskStatus = "ready"
	TaskStatusFailed     AudioTaskStatus = "failed"
)

// AudioTask 是 TTS 合成任务（对应表 audio_task）。
type AudioTask struct {
	ID          int64
	ArticleID   int64
	Voice       string
	Speed       float64
	Status      AudioTaskStatus
	Provider    string
	AudioID     int64 // ready 后回填
	ErrorCode   string
	ErrorMsg    string
	RetryCount  int
	NextRetryAt int64 // 退避重投时间（0 表示不等待）
	TextChars   int
	CreatedAt   int64
	UpdatedAt   int64
}

// Audio 是已落盘的音频（对应表 audio）。
type Audio struct {
	ID           int64
	TaskID       int64 // 可能为 0（task 被清理时 SET NULL）
	ArticleID    int64
	Voice        string
	Speed        float64
	Format       string
	FilePath     string // 相对 audioDir 的路径
	SizeBytes    int64
	DurationMs   int64
	SampleRate   int
	Provider     string
	HitCount     int64
	LastAccessAt int64 // LRU 依据
	CreatedAt    int64
}

// AudioCacheKey 是音频缓存键 (articleID, voice, speed)。
type AudioCacheKey struct {
	ArticleID int64
	Voice     string
	Speed     float64
}
