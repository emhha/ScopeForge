.PHONY: build build-web test clean
build: build-web

web/node_modules/.scopeforge-deps: web/package.json web/package-lock.json
	cd web && HUSKY=0 npm ci --no-audit --no-fund
	touch $@

build-web: web/node_modules/.scopeforge-deps
	cd web && npm run build

test: web/node_modules/.scopeforge-deps
	cd web && npm run typecheck && node --test src/lib/*.test.mjs

clean:
	$(RM) -r web/out web/tsconfig.tsbuildinfo
