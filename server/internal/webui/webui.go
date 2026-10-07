// Package webui 以内嵌文件系统的方式承载 EZNews 的 Web 端（含管理后台）。
//
// # 为什么要内嵌
//
// 部署目标是**一个可执行文件**：Web 端 pm2/nginx、服务端单独一个进程的双服务编排
// 对这个项目（自托管、轻量、省资源）来说没有收益，只会让部署步骤从 1 步变成
// 至少 4 步（起前端、起后端、配反代、配静态目录）。把 frontend 产物 embed 进
// 二进制后，用户拿到的就是一个 exe：双击运行，浏览器打开就是完整产品。
//
// # 为什么 dist 必须放在本目录
//
// Go 的 //go:embed **无法跨目录嵌入**（".." 开头的路径非法）。
// 同一个坑本仓库已踩过一次 —— 见 internal/migrations/migrations.go 的注释，
// 那里为了让 cmd 能拿到 SQL 迁移脚本，不得不在 migrations 目录里额外放一个
// Go 文件导出 embed.FS。这里同理：真正的构建产物在 web/dist，
// 由 Makefile / Dockerfile 在编译前**拷贝**到本目录的 dist/ 下。
//
// # 为什么 embed 的不是空目录
//
// //go:embed 对空目录会因无匹配文件而编译失败。因此本目录提交了一个占位的
// index.html（内含 EZNEWS_PLACEHOLDER 标记）。这带来三个好处：
//
//  1. 纯后端开发者 clone 下来直接 `go build` 就能成功，不必先装 Node / pnpm；
//  2. 交叉编译时不会因为某边缺少前端产物而失败；
//  3. 运行时可以据此区分"确实没内嵌前端"，从而给出明确提示而不是 404 或白屏。
//
// 发布产物由 Makefile 的 `make build-all` 或 Dockerfile 多阶段构建产出，
// 那两条路径都会先用真实构建产物覆盖 dist/，此时 HasUI() 返回 true。
package webui

import (
	"bytes"
	"embed"
	"io/fs"
)

// content 是外部 Web 端（含 /admin 管理后台）的构建产物。
//
// all: 前缀必需的：dist 里有 .vite/ 等以点开头的元数据目录，
// 普通的 //go:embed dist 会跳过它们（也会跳过隐藏文件）。
//
//go:embed all:dist
var content embed.FS

// Dist 返回 Web 端构建产物的子文件系统（根为 dist/ 目录内容）。
func Dist() (fs.FS, error) {
	return fs.Sub(content, "dist")
}

// placeholderMarker 是占位 index.html 里唯一的字符串标记。
// 运行时用它判断用户手上的是"还没内嵌前端"的裸服务端，还是完整发布产物。
//
// ★ 这个标记必须与 dist/index.html 占位文件保持完全一致，
//
//	否则 HasUI() 会把占位版本误判成完整产物 —— 用户打开就是一片空白。
const placeholderMarker = "EZNEWS_PLACEHOLDER"

// bundleMarkerSize 是占位文件的体积上限。
// 真实的 Vite 产物 index.html 通常几十到几百字节，但一定带 <script src="/assets/...">，
// 所以这里给一个宽松的阈值，配合内容匹配一起判断即可。
const bundleMarkerSize = 1024

// HasUI 判断本二进制是否真的内嵌了 Web 端产物。
//
// 只做这一个判断而不是直接判文件存在：占位文件是**必定存在**的
// （否则编译都不通过），所以"存在"说明不了任何事。
func HasUI() bool {
	f, err := content.ReadFile("dist/index.html")
	if err != nil {
		return false
	}
	if len(f) > bundleMarkerSize {
		// 远超占位体积，必然是真实产物。
		return true
	}
	return !bytes.Contains(f, []byte(placeholderMarker))
}
