module github.com/emhha/scopeforge/components/semantic-worker

go 1.26.3

require (
	github.com/daulet/tokenizers v1.27.0
	github.com/emhha/scopeforge v0.0.0
	github.com/yalue/onnxruntime_go v1.26.0
	trpc.group/trpc-go/trpc-agent-go v1.13.0-beta.1.0.20261111095122-90b8c46f000e
	trpc.group/trpc-go/trpc-agent-go/knowledge/vectorstore/sqlitevec v1.12.1-0.20261111095122-90b8c46f000e
	trpc.group/trpc-go/trpc-agent-go/memory/sqlitevec v1.12.1-0.20261111095122-90b8c46f000e
)

require (
	github.com/asg017/sqlite-vec-go-bindings v0.1.6 // indirect
	github.com/go-ego/gse v1.0.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-sqlite3 v1.14.32 // indirect
	github.com/ncruces/go-sqlite3 v0.32.0 // indirect
	github.com/ncruces/julianday v1.0.0 // indirect
	github.com/tetratelabs/wazero v1.11.0 // indirect
	github.com/vcaesar/cedar v0.20.2 // indirect
	go.opentelemetry.io/otel v1.29.0 // indirect
	go.opentelemetry.io/otel/trace v1.29.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.27.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	trpc.group/trpc-go/trpc-a2a-go v0.2.6-0.20260721084546-18c8244d0acb // indirect
)

replace github.com/emhha/scopeforge => ../..
