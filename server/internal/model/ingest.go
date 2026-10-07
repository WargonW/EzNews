package model

// IngestStatus 是单条 ingest 回执状态（三态 + 失败）。
type IngestStatus string

// 回执状态枚举（ARCHITECTURE.md §3.4.2）。
const (
	IngestStatusCreated IngestStatus = "created"
	IngestStatusUpdated IngestStatus = "updated"
	IngestStatusSkipped IngestStatus = "skipped"
	IngestStatusFailed  IngestStatus = "failed"
)

// IngestReason 是回执中的具体原因。
type IngestReason string

// 回执原因枚举。
const (
	ReasonUnchanged      IngestReason = "UNCHANGED"
	ReasonValidation     IngestReason = "VALIDATION_FAILED"
	ReasonSourceNotFound IngestReason = "SOURCE_NOT_FOUND"
	ReasonDedupConflict  IngestReason = "DEDUP_CONFLICT"
	ReasonInternal       IngestReason = "INTERNAL"
	ReasonUnprocessable  IngestReason = "UNPROCESSABLE"
)

// IngestItem 是采集器提交的标准化文章条目（同时作为 JSON 契约载体）。
//
// 语义约束：sourceKey 与 sourceId 至少提供其一；两者都提供且不一致 → VALIDATION_FAILED。
type IngestItem struct {
	SourceKey   string   `json:"sourceKey,omitempty"`
	SourceID    int64    `json:"sourceId,omitempty"`
	ExternalID  string   `json:"externalId,omitempty"`
	URL         string   `json:"url"`
	Title       string   `json:"title"`
	Summary     string   `json:"summary,omitempty"`
	Content     string   `json:"content,omitempty"`
	Category    string   `json:"category,omitempty"`
	Author      string   `json:"author,omitempty"`
	ImageURL    string   `json:"imageUrl,omitempty"`
	PublishedAt string   `json:"publishedAt"`
	Language    string   `json:"language,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// IngestResult 是单条回执明细。
type IngestResult struct {
	Index      int           `json:"index"`
	Status     IngestStatus  `json:"status"`
	Reason     *IngestReason `json:"reason,omitempty"`
	Field      *string       `json:"field,omitempty"`
	Message    *string       `json:"message,omitempty"`
	ArticleID  *int64        `json:"articleId,omitempty"`
	ExternalID *string       `json:"externalId,omitempty"`
	URL        *string       `json:"url,omitempty"`
}

// IngestReceipt 是批量 ingest 回执。恒等式：created + updated + skipped + failed = received。
type IngestReceipt struct {
	Received   int            `json:"received"`
	Created    int            `json:"created"`
	Updated    int            `json:"updated"`
	Skipped    int            `json:"skipped"`
	Failed     int            `json:"failed"`
	DurationMs int64          `json:"durationMs"`
	Results    []IngestResult `json:"results"`
}

// IngestBatchRequest 是批量接口的请求体。
type IngestBatchRequest struct {
	Items []IngestItem `json:"items"`
}
