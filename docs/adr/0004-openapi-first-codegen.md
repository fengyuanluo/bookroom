# 以 OpenAPI 规格为准，手写代码只写业务逻辑

`api/openapi.yaml` 是唯一的契约来源，其余代码从它和 SQL 生成：

- Go 接口与类型：oapi-codegen v2 的 `strict-server` + `gin`。
- 数据访问：sqlc。
- 迁移：goose。
- Readest fork 的客户端：openapi-typescript + openapi-fetch，替换现有手写的 fetch。

错误响应统一用 RFC 9457 `application/problem+json`。暂不做管理 UI，扫描、刮削、书单编辑都通过 API 完成；Agent 直接读规格，不额外生成 MCP。

## 考虑过的方案

- huma（code-first，运行时生成规格）：规格变成产物，不能先审再写，与"规格即契约、Agent 读规格"的目标相反。
- 手写 handler 加文档：两边容易漂移，客户端类型也得手写。

## 后果

- oapi-codegen v2.9 要求 Go 1.26 及以上。
- 生成的代码提交进仓库；CI 用 oasdiff 检查规格是否有破坏性变更，只允许新增字段和端点。
