.PHONY: run build test test-db vet fmt tidy migrate-up migrate-down

BINARY := bin/server
DSN ?= postgres://postgres:123456@localhost:5432/mini_agent_backend?sslmode=disable
TEST_DSN ?= host=localhost user=postgres password=123456 dbname=mini_agent_test port=5432 sslmode=disable

run:
	go run ./cmd/server

build:
	go build -o $(BINARY) ./cmd/server

# 不带数据库。repository / service 里需要 DB 的用例会被跳过 —— 这次是**说好的**,
# 所以显式带上 ALLOW_SKIP_DB=1。少了它那两个包会直接失败,理由见
# internal/service/main_test.go 的 requireDBOrExplicitSkip:
# go test 非 -v 模式下,"全跳过"和"全通过"输出完全一样。
test:
	@echo ">>> 不跑数据库用例(repository / service 里的大部分会被跳过)。真跑一遍: make test-db"
	ALLOW_SKIP_DB=1 go test ./...

# 带数据库,真跑仓库层用例
test-db:
	TEST_DSN="$(TEST_DSN)" go test ./... -v

# CI 用:不给逃生口。没有 TEST_DSN 就直接红,不会以"全跳过"的方式假装通过。
test-ci:
	TEST_DSN="$(TEST_DSN)" go test ./...

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
