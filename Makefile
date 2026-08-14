.PHONY: generate setup-agent test test-python test-race test-integration vet run

GOCACHE ?= /tmp/ailuo-gocache
UV ?= uv
PYTHON := $(UV) run --project agent --locked python

generate:
	PATH="$$(go env GOPATH)/bin:$$PATH" protoc --go_out=. --go_opt=module=github.com/projectluojia/AI-Luo-Man-ga --go-grpc_out=. --go-grpc_opt=module=github.com/projectluojia/AI-Luo-Man-ga proto/executor.proto proto/runtime_host.proto
	$(PYTHON) -m grpc_tools.protoc -I proto --python_out=agent/generated --grpc_python_out=agent/generated proto/executor.proto

setup-agent:
	$(UV) sync --project agent --locked

test:
	GOCACHE=$(GOCACHE) go test ./...
	$(MAKE) test-python

test-python:
	$(PYTHON) -m compileall -q agent
	$(PYTHON) -m unittest discover -s agent -p 'test_*.py' -v

test-race:
	GOCACHE=$(GOCACHE) go test -race ./...

test-integration:
	GOCACHE=$(GOCACHE) go test -tags=integration ./internal/kernel/loader -v -timeout=30s
	GOCACHE=$(GOCACHE) go test -tags=integration ./e2e -v -timeout=30s

vet:
	GOCACHE=$(GOCACHE) go vet ./...

run:
	GOCACHE=$(GOCACHE) go run .
