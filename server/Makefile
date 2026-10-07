# EZNews Server — 常用目标
# 注意：所有构建产物与模块缓存均落在 D 盘（GOPATH/GOCACHE 已指向 D:\dev\softwares\go-data）。

GO       ?= go
BINARY   := eznews
CMD      := ./cmd/eznews
OUTDIR   := ./bin

# ---- Web 端（含管理后台）产物 ----
# 目标是「一个可执行文件就能部署」，所以 Web 端产物必须在编译前拷进
# server 包内、由 //go:embed 直接打进二进制。
#
# ★ go:embed 不能跨目录嵌入，所以必须拷 ---- 不能让 Go 直接引用 ../web/dist。
#   同类约束本仓库已踩过一次：internal/migrations/migrations.go。
WEB_DIR      := ../web
WEB_DIST     := $(WEB_DIR)/dist
EMBED_DIST   := ./internal/webui/dist
PNPM         ?= pnpm

.PHONY: all build web ui-embed build-all run test vet fmt lint tidy clean smoke docker clean-ui

all: fmt vet build

# build 刻意不依赖前端产物：纯后端改动不需要为了编译而先跑一次 pnpm build，
# 尤其是 clone 下来还没装 Node 的环境也应当能直接 `make build` 成功。
build:
	@mkdir -p $(OUTDIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o $(OUTDIR)/$(BINARY) $(CMD)

# web 只负责产出前端静态产物。
web:
	cd $(WEB_DIR) && $(PNPM) install --frozen-lockfile && $(PNPM) run build

# ui-embed 把前端产物覆盖到 go:embed 读取的目录。
#
# ★ 用 rsync/cp -r 覆盖而非「先清空再拷」：
#   清空目录的瞬间如果有别的命令正在读取，会拿到不完整的视图；
#   internal/webui/dist/index.html 是**必须存在**的占位文件（embed 空目录会编译失败），
#   清空操作一旦中途失败，仓库就处于编译不过的状态。
ui-embed:
	@if [ ! -f "$(WEB_DIST)/index.html" ]; then \
		echo "未找到前端产物 $(WEB_DIST)/index.html，请先执行: make web"; \
		exit 1; \
	fi
	@printf '%s\n' "$(BINARY): 覆盖 $(EMBED_DIST) 为真实前端产物"
	cp -R "$(WEB_DIST)/." "$(EMBED_DIST)/"

# build-all 产出**完整的单文件可执行文件**（服务端 + Web 端 + 管理后台）。
# 这是发布时应该用的目标。
build-all: web ui-embed build
	@printf '%s\n' "$(BINARY): 已产出单文件可执行程序 $(OUTDIR)/$(BINARY)"

# clean-ui 把被真实产物覆盖掉的 dist 还原成仓库里的占位版本。
#
# dist/ 的内容是构建产物，不该长期留在源码里 —— 里保留的只有一个占位的
# index.html（保证纯后端 `go build` 也能过）。跑过 build-all 之后 dist/ 会被
# 真实产物污染，本目标负责还原。
#
# ★ 刻意不依赖 git：本工程未必一定是 git 仓库（也可能被归档成 zip 分发），
#   依赖 `git checkout` 会让这个目标在那种环境下直接失败。
#   占位内容另存为同目录下的 .placeholder.html，还原就是一次复制。
clean-ui:
	find "$(EMBED_DIST)" -mindepth 1 ! -name '.placeholder*' -delete
	cp "$(EMBED_DIST)/../.placeholder.html" "$(EMBED_DIST)/index.html"

run:
	$(GO) run $(CMD) -config ./config.yaml

test:
	$(GO) test ./... -count=1

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy

lint: vet
	@command -v gofmt >/dev/null 2>&1 && test -z "$$(gofmt -l .)" || (echo "gofmt 发现未格式化文件:"; gofmt -l .; exit 1)

clean:
	rm -rf $(OUTDIR)

# 端到端冒烟：起真实服务 → 打全部端点 → 收集日志断言 → 关服务。
# 二进制路径可用 SMOKE_BIN 覆盖；默认用 D 盘产物目录（与 build 目标一致需先 make build）。
SMOKE_BIN ?= /d/dev/softwares/eznews-data/bin/eznews
smoke: build
	@if [ ! -x "$(SMOKE_BIN)" ]; then \
		echo "冒烟需要预构建二进制：$(SMOKE_BIN)"; \
		echo "请先执行: make build OUTDIR=/d/dev/softwares/eznews-data/bin"; \
		exit 1; \
	fi
	SMOKE_BIN="$(SMOKE_BIN)" bash ./smoke/run.sh

docker:
	@echo "本工程不使用容器部署。"
	@echo "部署方式就是运行那一个可执行文件："
	@echo "    make build-all   # 产出 $(OUTDIR)/$(BINARY)"
	@echo "    ./$(OUTDIR)/$(BINARY) -config ./config.yaml"
