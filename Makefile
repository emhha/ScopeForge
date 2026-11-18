.DEFAULT_GOAL := build
GO ?= go
NPM ?= npm
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BINARY ?= artex
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build build-backend build-web release run dev-web test test-go test-web clean help
build: release

build-backend:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/artex

web/node_modules/.scopeforge-deps: web/package.json web/package-lock.json
	cd web && HUSKY=0 $(NPM) ci --no-audit --no-fund
	touch $@

build-web: web/node_modules/.scopeforge-deps
	cd web && $(NPM) run build

release: build-web
	$(RM) -r server/webui/dist
	mkdir -p server/webui/dist
	cp -R web/out/. server/webui/dist/
	$(GO) build -trimpath -tags embedui -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/artex

run:
	$(GO) run ./cmd/artex $(ARGS)

dev-web: web/node_modules/.scopeforge-deps
	cd web && $(NPM) run dev

test: test-go test-web

test-go:
	$(GO) test ./...

test-web: web/node_modules/.scopeforge-deps
	cd web && $(NPM) run typecheck && node --test src/lib/*.test.mjs

clean:
	$(RM) $(BINARY)
	$(RM) -r web/out server/webui/dist web/tsconfig.tsbuildinfo

help:
	@echo 'make build      构建内嵌前端的完整程序'
	@echo 'make release    构建内嵌前端的单文件程序'
	@echo 'make run        开发运行后端（ARGS=...）'
	@echo 'make dev-web    启动前端开发服务'
	@echo 'make test       Go 测试与前端检查'
	@echo 'make clean      清理构建产物'
