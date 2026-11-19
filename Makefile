.DEFAULT_GOAL := build
GO ?= go
GO_RUN = ./scripts/go-netloom.sh $(GO)
NPM ?= npm
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BINARY ?= dist/scopeforge$(EXE)
LDFLAGS := -s -w -X main.version=$(VERSION)
TARGET_OS := $(if $(GOOS),$(GOOS),$(shell $(GO) env GOHOSTOS))
TARGET_ARCH := $(if $(GOARCH),$(GOARCH),$(shell $(GO) env GOHOSTARCH))
BUILD_TARGETS := linux/amd64 linux/arm64 windows/amd64 windows/arm64 darwin/amd64 darwin/arm64
EXE := $(if $(filter windows,$(TARGET_OS)),.exe,)
PACKAGE_VERSION := $(patsubst v%,%,$(VERSION))
PACKAGE_NAME := scopeforge-$(PACKAGE_VERSION)-$(TARGET_OS)-$(TARGET_ARCH)
PACKAGE_DIR := dist/$(PACKAGE_NAME)

.PHONY: build all build-all build-backend build-web embed-web build-embedded release package run dev-web test test-go test-web clean help
# "make build all" is two Make goals; both share one build-all dependency.
ifneq ($(filter all build-all,$(MAKECMDGOALS)),)
build: build-all
else
build: release
endif

all: build-all

build-all: embed-web
	@set -e; \
	for target in $(BUILD_TARGETS); do \
		build_os=$${target%/*}; build_arch=$${target#*/}; build_ext=; \
		if [ "$$build_os" = windows ]; then build_ext=.exe; fi; \
		$(MAKE) build-embedded GOOS="$$build_os" GOARCH="$$build_arch" \
			BINARY="dist/scopeforge-$$build_os-$$build_arch$$build_ext"; \
	done

build-backend:
	mkdir -p $(dir $(BINARY))
	GOOS=$(TARGET_OS) GOARCH=$(TARGET_ARCH) CGO_ENABLED=0 $(GO_RUN) build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/scopeforge

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
	GOOS=$(TARGET_OS) GOARCH=$(TARGET_ARCH) CGO_ENABLED=0 $(GO_RUN) build -trimpath -tags embedui -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/scopeforge

release: embed-web
	$(MAKE) build-embedded

# Uses existing frontend artifacts; run make embed-web first for a local package.
package:
	@case '$(PACKAGE_VERSION)' in ''|*[!A-Za-z0-9._+-]*) echo 'Invalid VERSION'; exit 1;; esac
	$(RM) -r '$(PACKAGE_DIR)'
	mkdir -p '$(PACKAGE_DIR)'
	$(MAKE) build-embedded BINARY='$(PACKAGE_DIR)/scopeforge$(EXE)'
	cp config.example.json README.md LICENSE '$(PACKAGE_DIR)/'
	cp -R skills '$(PACKAGE_DIR)/skills'
	$(RM) 'dist/$(PACKAGE_NAME).zip'
	cd dist && zip -qr '$(PACKAGE_NAME).zip' '$(PACKAGE_NAME)'

run:
	$(GO_RUN) run ./cmd/scopeforge $(ARGS)

dev-web: web/node_modules/.scopeforge-deps
	cd web && $(NPM) run dev

test: test-go test-web

test-go:
	$(GO_RUN) test ./...

test-web: web/node_modules/.scopeforge-deps
	cd web && $(NPM) run typecheck && node --test src/lib/*.test.mjs

clean:
	$(RM) $(BINARY)
	$(RM) -r dist web/out server/webui/dist web/tsconfig.tsbuildinfo

help:
	@echo 'make build      构建当前系统程序到 dist/scopeforge（Windows 为 scopeforge.exe）'
	@echo 'make build all  构建 Linux / Windows / macOS 的 amd64、arm64 到 dist/'
	@echo 'make release    构建内嵌前端程序到 dist/scopeforge'
	@echo 'make package    用已构建前端打包 zip（支持 GOOS / GOARCH / VERSION）'
	@echo 'make run        开发运行后端（ARGS=...）'
	@echo 'make dev-web    启动前端开发服务'
	@echo 'make test       Go 测试与前端检查'
	@echo 'make clean      清理构建产物'
