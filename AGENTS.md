# AGENTS.md

BookRoom：Go + Gin + SQLite 的自托管书库服务端，兼容 Readest 云契约、KOSync 与 OPDS 1.2。先读 `docs/architecture.md` 与 `GLOSSARY.md`。

## 必须遵守

- `api/openapi.yaml` 是接口唯一真相源。改接口先改规格，再 `make generate`，最后实现 `internal/httpapi` 里的方法。绝不手改 `internal/api/api.gen.go` 或 `internal/store/db/*`。
- 规格只做向后兼容的新增；Readest 兼容端点（`/api/sync*`、`/api/storage/*`、`/auth/v1/*`）、`/kosync/*`、`/opds*` 的线格式由外部客户端决定，改动前读 `docs/compatibility.md` 和对应 ADR。
- Readest 合并规则在 `internal/readest/merge.go`，函数名与 Readest `pages/api/sync.ts` 一一对应；改规则必须说明依据的上游版本与 issue。
- handler 调服务时传 `rc(ctx)`，不要把 `*gin.Context` 传进数据库或 HTTP 客户端。
- 服务返回 `apperr` 表达预期失败；不要在服务里写 HTTP 响应。
- 新的设计取舍写 ADR；术语变化同步 `GLOSSARY.md`（只写术语，不写实现）。

## 命令

```bash
make generate        # 规格或 SQL 改动后
make test            # 全部测试
make check           # 提交前：vet、gofmt、生成代码无漂移、race 测试
make bench           # 性能相关改动前后对比
```

## 验证

- 端点改动：在 `internal/httpapi` 写端到端测试（`newEnv` + `e.call` + `e.expect`），外部服务用 `httptest` 假冒。
- 合并规则改动：在 `internal/readest/merge_test.go` 覆盖新分支。
- 查询改动：`make bench` 看两万本书时的耗时没有回退。
