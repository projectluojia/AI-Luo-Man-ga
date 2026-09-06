.PHONY: generate test test-agent test-race test-integration vet run

GOCACHE ?= /tmp/ailuo-gocache
UV ?= uv
AGENT_PROJECT := packages/agent/runtime
AGENT_PYTHON := $(UV) run --project $(AGENT_PROJECT) --locked python

generate:
	$(UV) sync --project $(AGENT_PROJECT) --locked
	$(AGENT_PYTHON) -m grpc_tools.protoc -I proto --go_out=. --go_opt=module=github.com/projectluojia/AI-Luo-Man-ga --go-grpc_out=. --go-grpc_opt=module=github.com/projectluojia/AI-Luo-Man-ga proto/executor.proto proto/runtime_host.proto
	$(AGENT_PYTHON) -m grpc_tools.protoc -I proto --python_out=$(AGENT_PROJECT)/agent/generated --grpc_python_out=$(AGENT_PROJECT)/agent/generated proto/executor.proto

test:
	GOCACHE=$(GOCACHE) go test ./...

test-agent:
	$(UV) sync --project $(AGENT_PROJECT) --locked
	$(AGENT_PYTHON) -m compileall -q $(AGENT_PROJECT)
	$(AGENT_PYTHON) -m unittest discover -s $(AGENT_PROJECT) -p 'test_*.py' -v

test-race:
	GOCACHE=$(GOCACHE) go test -race ./...

test-integration:
	GOCACHE=$(GOCACHE) go test -tags=integration ./internal/kernel/loader -v -timeout=30s

vet:
	GOCACHE=$(GOCACHE) go vet ./...

run:
	GOCACHE=$(GOCACHE) go run .
