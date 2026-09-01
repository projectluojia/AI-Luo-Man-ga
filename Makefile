.PHONY: generate test test-contracts test-package-manager test-agent test-campus test-e2e test-race test-integration vet run

UV ?= uv
AGENT_PROJECT := packages/agent/runtime
AGENT_PACKAGE := packages/agent
AGENT_PYTHON := $(UV) run --project $(AGENT_PROJECT) --locked python

generate:
	@command -v protoc-gen-go >/dev/null 2>&1 || { echo "缺少 protoc-gen-go，请先安装 google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12" >&2; exit 1; }
	@command -v protoc-gen-go-grpc >/dev/null 2>&1 || { echo "缺少 protoc-gen-go-grpc，请先安装 google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2" >&2; exit 1; }
	$(UV) sync --project $(AGENT_PROJECT) --locked
	$(AGENT_PYTHON) -m grpc_tools.protoc -I proto --go_out=. --go_opt=module=github.com/projectluojia/AI-Luo-Man-ga --go-grpc_out=. --go-grpc_opt=module=github.com/projectluojia/AI-Luo-Man-ga proto/executor.proto proto/runtime_host.proto
	$(AGENT_PYTHON) -m grpc_tools.protoc -I proto --python_out=$(AGENT_PROJECT)/agent/generated --grpc_python_out=$(AGENT_PROJECT)/agent/generated proto/executor.proto

test:
	go test ./...
	$(MAKE) test-contracts

test-contracts:
	cd contracts && go test ./...

test-package-manager:
	cd package-manager && go test ./...

test-agent:
	$(UV) sync --project $(AGENT_PROJECT) --locked
	$(AGENT_PYTHON) -m compileall -q $(AGENT_PROJECT)
	cd $(AGENT_PROJECT) && $(UV) run --project . --locked ruff check .
	$(AGENT_PYTHON) -m unittest discover -s $(AGENT_PROJECT) -p 'test_*.py' -v

test-campus:
	cd packages/campus-bus && go mod verify && go mod tidy -diff && GOOS=wasip1 GOARCH=wasm go vet ./src && GOOS=wasip1 GOARCH=wasm go build -trimpath -o campus.wasm ./src

test-e2e:
	$(UV) sync --project $(AGENT_PROJECT) --locked
	AILUO_EXECUTOR_PACKAGE_DIR=$(AGENT_PACKAGE) go test -tags=integration ./e2e -v -timeout=60s

test-race:
	go test -race ./...

test-integration:
	go test -tags=integration ./internal/kernel/loader -v -timeout=30s

vet:
	go vet ./...

run:
	go run .
