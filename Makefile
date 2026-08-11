.PHONY: run build test test-db vet fmt tidy migrate-up migrate-down

BINARY := bin/server
DSN ?= postgres://postgres:123456@localhost:5432/mini_agent_backend?sslmode=disable
TEST_DSN ?= host=localhost user=postgres password=123456 dbname=mini_agent_test port=5432 sslmode=disable

run:
	go run ./cmd/server

build:
	go build -o $(BINARY) ./cmd/server

# 不带数据库,需要 DB 的用例会跳过
test:
	go test ./...

# 带数据库,真跑仓库层用例
test-db:
	TEST_DSN="$(TEST_DSN)" go test ./... -v

vet:
	go vet ./...

fmt:
	gofmt -l -w .

tidy:
	go mod tidy

migrate-up:
	migrate -path migrations -database "$(DSN)" up

migrate-down:
	migrate -path migrations -database "$(DSN)" down 1

.PHONY: proto
proto:
	protoc --proto_path=proto --go_out=gen --go_opt=paths=source_relative --go-grpc_out=gen --go-grpc_opt=paths=source_relative  proto/agent/v1/agent.proto
