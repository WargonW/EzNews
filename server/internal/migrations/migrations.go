// Package migrations 以 go:embed 内嵌 SQL 迁移脚本。
//
// 说明（对 ARCHITECTURE.md §14.1 文件清单的微调）：清单中该目录只列了 .sql 文件，
// 但 Go 的 //go:embed 无法跨目录嵌入，因此本目录额外提供一个 Go 文件导出 embed.FS，
// 迁移执行器仍在 internal/store/migrate.go 中。
package migrations

import (
	"embed"
	"sort"
	"strconv"
	"strings"
)

//go:embed *.sql
var FS embed.FS

// Migration 描述一个已排序的迁移脚本。
type Migration struct {
	Version int    // 文件名前缀的数字版本号
	Name    string // 文件名
	SQL     string // 脚本内容
}

// List 返回按版本号升序排列的全部迁移脚本。
func List() ([]Migration, error) {
	entries, err := FS.ReadDir(".")
	if err != nil {
		return nil, err
	}
	out := make([]Migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		verStr, _, _ := strings.Cut(e.Name(), "_")
		ver, err := strconv.Atoi(verStr)
		if err != nil {
			continue
		}
		data, err := FS.ReadFile(e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, Migration{Version: ver, Name: e.Name(), SQL: string(data)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}
