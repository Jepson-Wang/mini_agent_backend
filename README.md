# mini_agent_backend

Agent 网关后端。

## 目录结构

```
cmd/server        唯一入口:读配置、初始化依赖、启动、优雅关闭
internal/
  config          配置结构体 + 加载
  model           数据模型 + 业务错误
  repository      数据访问,只跟 DB 打交道
  service         业务逻辑,事务边界在这一层
  handler         HTTP 路由与 DTO
  middleware      TraceID / Recovery / Auth / Logger
  pkg             项目内共享工具
  agent           Agent 侧 gRPC 客户端封装
migrations        golang-migrate 的目录。**目前是空占位**(两个文件各只有一行注释),
                  表结构的真实来源是 cmd/server 里的 db.AutoMigrate
gen               protoc 产物,不入库(见下面「生成 gRPC 代码」)
configs           配置文件
```

前端在仓库外面的 `../frontend/`(Vite + React)。它靠 dev proxy 转发 `/api` 和
`/health` 到本服务 —— **本服务没有 CORS 中间件**,浏览器直连会在预检就被拦掉。

## 开发

先准备环境变量。`JWT_SECRET` 没有默认值也不能写进 `configs/config.yaml`,不给就起不来:

```
cp .env.example .env
openssl rand -base64 48     # 把结果填进 .env 的 JWT_SECRET
```

`.env` 只在服务启动时加载,且不覆盖已存在的环境变量。
优先级:内置默认值 < `configs/config.yaml` < `.env` < 真实环境变量。

```
make run       启动服务
make test      跑测试(不连库,需要 DB 的用例会跳过)
make test-db   跑测试(连库,真跑仓库层和 service 层)
make test-ci   跑测试(不给逃生口:没有 TEST_DSN 直接红)
make vet       go vet ./...
make fmt       gofmt -l -w .
make proto     重新生成 gRPC 代码
```

裸 `go test ./...` 在没有 `TEST_DSN` 时会让 `repository` 和 `service` 两个包**直接失败**,
这是刻意的:`go test` 不带 `-v` 时既不打印 skip、也不打印通过的包的输出,
"用例全被跳过"和"全部通过"在终端上都是一行 `ok`。要在没有库的情况下跑就显式说出来 ——
`make test` 会替你带上 `ALLOW_SKIP_DB=1`。

## 生成 gRPC 代码

`gen/agent/v1/*.pb.go` 是 protoc 的产物,**没有入库**(`.gitignore` 里的 `*.pb.go`)。
所以新克隆下来之后 `go build ./...` 会失败,先跑一次:

```
make proto
```

`proto/agent/v1/agent.proto` 和 Agent 仓库里的那一份必须逐字节相同 —— 它是两边共同的契约。
