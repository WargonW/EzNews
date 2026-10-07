package repo

// chunkStrings 将字符串切片按 size 分批（用于 IN 查询分片，避免 SQL 过长）。
func chunkStrings(in []string, size int) [][]string {
	if size <= 0 {
		size = 500
	}
	var out [][]string
	for start := 0; start < len(in); start += size {
		end := start + size
		if end > len(in) {
			end = len(in)
		}
		out = append(out, in[start:end])
	}
	return out
}

// chunkInt64 将 int64 切片按 size 分批。
func chunkInt64(in []int64, size int) [][]int64 {
	if size <= 0 {
		size = 500
	}
	var out [][]int64
	for start := 0; start < len(in); start += size {
		end := start + size
		if end > len(in) {
			end = len(in)
		}
		out = append(out, in[start:end])
	}
	return out
}

// toAnySlice 将 []string 转为 []any（用于 SQL 参数展开）。
func toAnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// toAnyInt64 将 []int64 转为 []any。
func toAnyInt64(in []int64) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}
