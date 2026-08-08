.PHONY: generate setup-agent test test-race test-integration vet run

GOCACHE ?= /tmp/ailuo-gocache

generate:
	PATH="$$(go env GOPATH)/bin:$$PATH" protoc --go_out=. --go_opt=module=github.com/projectluojia/AI-Luo-Man-ga --go-grpc_out=. --go-grpc_opt=module=github.com/projectluojia/AI-Luo-Man-ga proto/agent.proto proto/runtime_host.proto
	agent/.venv/bin/python -m grpc_tools.protoc -I proto --python_out=agent/generated --grpc_python_out=agent/generated proto/agent.proto

setup-agent:
	python3 -m venv agent/.venv
	agent/.venv/bin/pip install -r agent/requirements.txt

test:
	GOCACHE=$(GOCACHE) go test ./...
	agent/.venv/bin/python -m unittest discover -s agent -p 'test_*.py' -v

test-race:
	GOCACHE=$(GOCACHE) go test -race ./...

test-integration:
	GOCACHE=$(GOCACHE) go test -tags=integration ./internal/kernel/loader -v -timeout=30s
	GOCACHE=$(GOCACHE) go test -tags=integration ./e2e -v -timeout=30s

vet:
	GOCACHE=$(GOCACHE) go vet ./...

run:
	GOCACHE=$(GOCACHE) go run .
