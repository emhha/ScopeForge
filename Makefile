.DEFAULT_GOAL := build
GO ?= go
NPM ?= npm
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BINARY ?= artex
LDFLAGS := -s -w -X main.version=$(VERSION)
TARGET_OS := $(shell $(GO) env GOOS)
TARGET_ARCH := $(shell $(GO) env GOARCH)
EXE := $(if $(filter windows,$(TARGET_OS)),.exe,)
PACKAGE_VERSION := $(patsubst v%,%,$(VERSION))
PACKAGE_NAME := artex-$(PACKAGE_VERSION)-$(TARGET_OS)-$(TARGET_ARCH)
PACKAGE_DIR := dist/$(PACKAGE_NAME)

.PHONY: build build-backend build-web embed-web build-embedded release package run dev-web test test-go test-web clean help
build: release

build-backend:
	mkdir -p $(dir $(BINARY))
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/artex

web/node_modules/.scopeforge-deps: web/package.json web/package-lock.json
	cd web && HUSKY=0 $(NPM) ci --no-audit --no-fund
	touch $@

build-web: web/node_modules/.scopeforge-deps
	cd web && $(NPM) run build

embed-web: build-web
	$(RM) -r server/webui/dist
	mkdir -p server/webui/dist
	cp -R web/out/. server/webui/dist/

build-embedded:
	test -f server/webui/dist/index.html
	mkdir -p $(dir $(BINARY))
	CGO_ENABLED=0 $(GO) build -trimpath -tags embedui -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/artex

release: embed-web
	$(MAKE) build-embedded

# Uses existing frontend artifacts; run make embed-web first for a local package.
package:
	@case '$(PACKAGE_VERSION)' in ''|*[!A-Za-z0-9._+-]*) echo 'Invalid VERSION'; exit 1;; esac
	$(RM) -r '$(PACKAGE_DIR)'
	mkdir -p '$(PACKAGE_DIR)'
	$(MAKE) build-embedded BINARY='$(PACKAGE_DIR)/artex$(EXE)'
	cp config.example.json README.md LICENSE '$(PACKAGE_DIR)/'
	cp -R skills '$(PACKAGE_DIR)/skills'
	$(RM) 'dist/$(PACKAGE_NAME).zip'
	cd dist && zip -qr '$(PACKAGE_NAME).zip' '$(PACKAGE_NAME)'

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
	$(RM) -r dist web/out server/webui/dist web/tsconfig.tsbuildinfo

help:
	@echo 'make build      构建内嵌前端的完整程序'
	@echo 'make release    构建内嵌前端的单文件程序'
	@echo 'make package    用已构建前端打包 zip（支持 GOOS / GOARCH / VERSION）'
	@echo 'make run        开发运行后端（ARGS=...）'
	@echo 'make dev-web    启动前端开发服务'
	@echo 'make test       Go 测试与前端检查'
	@echo 'make clean      清理构建产物'
