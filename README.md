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
migrations        手写 DDL
configs           配置文件
```

## 开发

先准备环境变量。`JWT_SECRET` 没有默认值也不能写进 `configs/config.yaml`,不给就起不来:

```
cp .env.example .env
openssl rand -base64 48     # 把结果填进 .env 的 JWT_SECRET
```

`.env` 只在服务启动时加载,且不覆盖已存在的环境变量。
优先级:内置默认值 < `configs/config.yaml` < `.env` < 真实环境变量。

```
make run      启动服务
make test     跑测试
make migrate  执行迁移
```
