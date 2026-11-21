.DEFAULT_GOAL := build
GO ?= go
GO_RUN = ./scripts/go-netloom.sh $(GO)
NPM ?= npm
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BINARY ?= dist/scopeforge$(EXE)
LDFLAGS := -s -w -X main.version=$(VERSION)
TARGET_OS := $(if $(GOOS),$(GOOS),$(shell $(GO) env GOHOSTOS))
TARGET_ARCH := $(if $(GOARCH),$(GOARCH),$(shell $(GO) env GOHOSTARCH))
WEB_TESTS := $(filter-out web/src/lib/finding-lineage-browser.test.mjs,$(wildcard web/src/lib/*.test.mjs web/src/features/conversation/*.test.mjs))
BUILD_TARGETS := linux/amd64 linux/arm64 windows/amd64 windows/arm64 darwin/amd64 darwin/arm64
EXE := $(if $(filter windows,$(TARGET_OS)),.exe,)
PACKAGE_VERSION := $(patsubst v%,%,$(VERSION))
PACKAGE_NAME := scopeforge-$(PACKAGE_VERSION)-$(TARGET_OS)-$(TARGET_ARCH)
PACKAGE_DIR := dist/$(PACKAGE_NAME)
SEMANTIC_PACKAGE ?= 0

.PHONY: build all build-all build-backend build-web embed-web build-embedded release package run dev-web test test-go test-web test-web-browser clean help semantic-component semantic-offline package-with-semantic
# "make build all" is two Make goals; both share one build-all dependency.
ifneq ($(filter all build-all,$(MAKECMDGOALS)),)
build: build-all
else
build: release
endif

all: build-all

# CGO requires a compiler for every target. Check all of them before building.
build-all:
	GO='$(GO)' NPM='$(NPM)' VERSION='$(VERSION)' ./scripts/build-all.sh $(BUILD_TARGETS)

build-backend:
	mkdir -p $(dir $(BINARY))
	GOOS=$(TARGET_OS) GOARCH=$(TARGET_ARCH) CGO_ENABLED=1 $(GO_RUN) build -trimpath -tags sqlite_fts5 -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/scopeforge

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
	GOOS=$(TARGET_OS) GOARCH=$(TARGET_ARCH) CGO_ENABLED=1 $(GO_RUN) build -trimpath -tags 'embedui sqlite_fts5' -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/scopeforge

release: embed-web
	$(MAKE) build-embedded

# Uses existing frontend artifacts; run make embed-web first for a local package.
package:
	@case '$(PACKAGE_VERSION)' in ''|*[!A-Za-z0-9._+-]*) echo 'Invalid VERSION'; exit 1;; esac
	$(RM) -r '$(PACKAGE_DIR)'
	mkdir -p '$(PACKAGE_DIR)'
	$(MAKE) build-embedded BINARY='$(PACKAGE_DIR)/scopeforge$(EXE)'
	cp config.example.json README.md MEMORY_KNOWLEDGE.md LICENSE '$(PACKAGE_DIR)/'
	cp server/modelcatalog/LICENSE '$(PACKAGE_DIR)/models.dev-LICENSE'
	cp -R skills '$(PACKAGE_DIR)/skills'
	@if [ '$(SEMANTIC_PACKAGE)' = 1 ]; then \
		python3 -c 'import json; c=json.load(open("dist/semantic/catalog.json")); assert c["platform"] == "$(TARGET_OS)/$(TARGET_ARCH)", "semantic component platform mismatch"'; \
		cp -R dist/semantic '$(PACKAGE_DIR)/semantic'; \
		$(RM) '$(PACKAGE_DIR)/semantic/model.onnx' '$(PACKAGE_DIR)/semantic/tokenizer.json'; \
	fi
	$(RM) 'dist/$(PACKAGE_NAME).zip'
	cd dist && zip -qr '$(PACKAGE_NAME).zip' '$(PACKAGE_NAME)'

# Optional native component: build on the target platform with a C compiler.
# The main binary does not download or load the optional inference runtime.
semantic-component:
	python3 scripts/package-semantic.py --go '$(GO)'

semantic-offline:
	python3 scripts/package-semantic.py --go '$(GO)' --offline

package-with-semantic: semantic-component embed-web
	$(MAKE) package SEMANTIC_PACKAGE=1

run:
	CGO_ENABLED=1 $(GO_RUN) run -tags sqlite_fts5 ./cmd/scopeforge $(ARGS)

dev-web: web/node_modules/.scopeforge-deps
	cd web && $(NPM) run dev

test: test-go test-web

test-go:
	CGO_ENABLED=1 $(GO_RUN) test -tags sqlite_fts5 ./...

test-web: web/node_modules/.scopeforge-deps
	cd web && $(NPM) run typecheck && node --test $(patsubst web/%,%,$(WEB_TESTS))

test-web-browser: web/node_modules/.scopeforge-deps
	cd web && node --test src/lib/finding-lineage-browser.test.mjs

clean:
	$(RM) $(BINARY)
	$(RM) -r dist web/out server/webui/dist web/tsconfig.tsbuildinfo

help:
	@echo 'make build      构建当前系统程序到 dist/scopeforge（Windows 为 scopeforge.exe）'
	@echo 'make build all  构建全部平台，需提前配置各目标的 C 编译器（见 README）'
	@echo 'make release    构建内嵌前端程序到 dist/scopeforge'
	@echo 'make package    用已构建前端打包 zip（支持 GOOS / GOARCH / VERSION）'
	@echo 'make package-with-semantic  打包含可选本地语义组件的当前平台发行包'
	@echo 'make semantic-offline      生成当前平台离线模型安装包'
	@echo 'make run        开发运行后端（ARGS=...）'
	@echo 'make dev-web    启动前端开发服务'
	@echo 'make test       Go 测试与前端检查'
	@echo 'make clean      清理构建产物'
