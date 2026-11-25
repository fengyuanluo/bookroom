# BookRoom

自托管书库服务端：书放在服务器上，任何设备登录即可看到书单、打开即读、进度和阅读时长跨设备同步。客户端 [BookRoom Reader](https://github.com/fengyuanluo/bookroom-client) 是 [Readest](https://github.com/readest/readest) 的硬分叉（桌面、Android 安装包在其 Releases）；KOReader、CrossPoint 等阅读器通过 KOSync 和 OPDS 接入。

单个 Go 二进制（Gin + SQLite，无 cgo），一个数据目录，零外部服务。

| 能力 | 怎么提供 |
|---|---|
| 书库与入库 | EPUB / PDF / CBZ / CBR / TXT / MOBI / AZW3 / FB2 等，按内容哈希（partialMD5）去重存储，读取文件内嵌元数据 |
| 书单 | 每个用户一套可嵌套文件夹（`小说/仙侠`），就是 Readest 首屏的分组 |
| 进度、批注、阅读统计 | 兼容 Readest 云的 `/api/sync` 线格式，Readest 与 `readest.koplugin` 不改数据层即可同步 |
| 客户端配置 | Readest 的 `/api/sync/replicas`（字体、词典、主题、翻译器等，HLC 字段级合并） |
| KOReader 原生同步 | KOSync 协议，设备密钥登录；EPUB 位置与 Readest 互通 |
| 其他阅读器 | OPDS 1.2 目录（导航、分页、搜索、封面） |
| 元数据刮削 | Google Books、Open Library、Hardcover、小说汇（网文），豆瓣需显式开启；后台只按 ISBN 或书名+作者精确匹配补缺 |
| 阅读时长 | 原始翻页事件入库，按日 / 按书 / 连续天数实时聚合 |
| Agent 友好 | 一份 OpenAPI 3 规格生成服务端骨架；`GET /openapi.json` 自描述；错误统一 RFC 9457 |

## 快速开始

```bash
make build
BOOKROOM_DATA_DIR=./data BOOKROOM_ADMIN_USER=admin BOOKROOM_ADMIN_PASSWORD='change-me-now' ./bin/bookroom serve
```

或用容器：

```bash
docker build -t bookroom .
docker run -d -p 8080:8080 -v bookroom-data:/data \
  -e BOOKROOM_ADMIN_USER=admin -e BOOKROOM_ADMIN_PASSWORD='change-me-now' bookroom
```

导入已有的书，再登录看看：

```bash
./bin/bookroom import -user admin -shelf 收件箱 ~/Books
TOKEN=$(curl -s localhost:8080/api/v1/auth/login -H 'content-type: application/json' \
  -d '{"username":"admin","password":"change-me-now"}' | jq -r .accessToken)
curl -s 'localhost:8080/api/v1/books?q=三体' -H "Authorization: Bearer $TOKEN" | jq '.items[].title'
```

KOReader：在 BookRoom 里 `POST /api/v1/devices` 生成设备密钥，KOSync 服务器填 `http://主机:8080/kosync`，密码填密钥；OPDS 目录填 `http://主机:8080/opds`。

浏览器访问：把 BookRoom Reader 的 Web 静态导出（`pnpm build-web` 产出的 `out/`）放到 `BOOKROOM_WEB_DIR`，打开 `http://主机:8080/` 即为登录页。

## 文档

| 文档 | 内容 |
|---|---|
| [docs/architecture.md](docs/architecture.md) | 模块划分、数据流、并发与安全模型、数据表 |
| [docs/api.md](docs/api.md) | API 约定、鉴权方式、示例、全部端点索引 |
| [docs/compatibility.md](docs/compatibility.md) | 与 Readest 云、readest.koplugin、KOSync、OPDS 的契约及差异；各客户端怎么接入 |
| [docs/operations.md](docs/operations.md) | 部署、全部配置项、数据目录、备份、反向代理、元数据源 |
| [docs/development.md](docs/development.md) | 代码生成流程、加端点 / 加迁移、测试与基准 |
| [docs/adr/](docs/adr/) | 架构决策记录（为什么这样设计） |
| [GLOSSARY.md](GLOSSARY.md) | 领域术语 |
| [api/openapi.yaml](api/openapi.yaml) | 唯一的接口契约 |
| [research/](research/) | 设计前的调研报告 |

## 许可

AGPL-3.0-or-later（客户端 Readest 为 AGPL-3.0）。
