# 运维

## 运行

```bash
# 二进制
make build
BOOKROOM_DATA_DIR=/srv/bookroom BOOKROOM_PUBLIC_URL=https://books.example.com ./bin/bookroom serve

# 容器（数据在 /data，进程以 nonroot 运行）
docker run -d --name bookroom -p 8080:8080 -v /srv/bookroom:/data \
  -e BOOKROOM_PUBLIC_URL=https://books.example.com \
  -e BOOKROOM_ADMIN_USER=admin -e BOOKROOM_ADMIN_PASSWORD='…' bookroom
```

首次启动且库里没有任何账号时，`BOOKROOM_ADMIN_USER`/`BOOKROOM_ADMIN_PASSWORD` 会创建管理员；之后这两个变量不再起作用。也可以用命令建号（见下文）。`SIGINT`/`SIGTERM` 会优雅关闭，最多等待 20 秒。

## 命令

| 命令 | 作用 |
|---|---|
| `bookroom serve` | 启动服务（默认命令） |
| `bookroom import [-user 名] [-shelf 路径] 文件或目录…` | 递归导入支持的格式，跳过隐藏文件；同盘时硬链接，否则复制；已在书库的跳过；`-shelf` 需配合 `-user` |
| `bookroom user add [-admin] [-email 地址] 名` | 建号，密码取 `BOOKROOM_PASSWORD` 或标准输入 |
| `bookroom user passwd 名` | 改密码（同时作废该用户所有刷新令牌） |
| `bookroom user list` | 列出账号 |
| `bookroom backup 文件` | 在线备份数据库（`VACUUM INTO`），目标文件不能已存在 |
| `bookroom version` | 版本 |

命令和服务共用同一套环境变量，可以在服务运行时执行（SQLite WAL 允许）。

## 配置

全部来自环境变量，前缀 `BOOKROOM_`。

| 变量 | 默认值 | 说明 |
|---|---|---|
| `ADDR` | `:8080` | 监听地址 |
| `DATA_DIR` | `./data` | 数据目录，自动创建 |
| `PUBLIC_URL` | 空 | 外部访问地址，用于签名下载链接和 OPDS 绝对链接；为空时取请求的 Host 及 `X-Forwarded-Proto/Host` |
| `WEB_DIR` | 空 | Readest Web 静态构建目录；设置后在 `/` 提供，未知路径回退到 `index.html` |
| `JWT_SECRET` | 自动生成 | ≥32 字符；为空时生成并保存到 `DATA_DIR/jwt.key`。更换会让所有访问令牌和签名链接失效 |
| `ACCESS_TTL` | `1h` | 访问令牌有效期 |
| `REFRESH_TTL` | `720h` | 刷新令牌有效期 |
| `ADMIN_USER` / `ADMIN_PASSWORD` | 空 | 首次启动时的管理员 |
| `CORS_ORIGINS` | `*` | 逗号分隔的允许来源 |
| `MAX_UPLOAD_BYTES` | `2147483648` | 单个上传上限 |
| `LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `LOG_FORMAT` | `text` | `text` 或 `json` |
| `PROXY_ALLOW_PRIVATE` | `false` | 允许代理访问私网地址（局域网里的 Komga、Calibre 等）；回环地址始终禁止 |
| `PROXY_TRUSTED_HOSTS` | 空 | 逗号分隔的主机名，代理访问它们时不做 SSRF 检查 |
| `SCRAPE_AUTO` | `true` | 入库后自动补缺元数据 |
| `SCRAPE_INTERVAL` | `2s` | 两个刮削任务之间的间隔 |
| `GOOGLE_BOOKS_KEY` | 空 | Google Books API key；不配置则不启用（无 key 的日配额为 0） |
| `HARDCOVER_TOKEN` | 空 | Hardcover 个人令牌 |
| `OPENLIBRARY_ENABLED` | `true` | Open Library 开关 |
| `OPENLIBRARY_CONTACT` | 空 | 联系邮箱；填写后放进 User-Agent，限速从 1 次/秒提高到 3 次/秒 |
| `DOUBAN_ENABLED` | `false` | 豆瓣（抓取网页，违反其条款，风险自负） |
| `XIAOSHUOHUB_ENABLED` | `true` | 小说汇开关 |
| `XIAOSHUOHUB_URL` | `https://api.xiaoshuohub.com` | 小说汇开发者 API 地址（[开发者文档](https://www.xiaoshuohub.com/developers/)） |
| `XIAOSHUOHUB_APP_KEY` / `XIAOSHUOHUB_APP_SECRET` | 空 | 应用凭据（在 `https://api.xiaoshuohub.com/apps/developer/` 创建）；不填则匿名调用（200 次/天/IP），填写后 1000 次/滚动 24 小时 |

## 数据目录

```
DATA_DIR/
├── bookroom.db (+ -wal, -shm)   全部元数据与同步数据
├── jwt.key                      自动生成的签名密钥
├── library/ab/<哈希>/<哈希>.epub 书文件，入库后不再改动
├── covers/ab/<哈希>.jpg          封面
├── objects/<用户ID>/…            Readest 每用户对象（字体、纹理等）
└── tmp/                         上传暂存，超过 24 小时自动清理
```

## 备份与恢复

1. `bookroom backup /backup/bookroom-$(date +%F).db`（服务运行中也一致）。
2. 同时备份 `library/`、`covers/`、`objects/` 和 `jwt.key`（文件只增不改，rsync 增量即可）。

恢复：停服务，把备份的数据库放回 `DATA_DIR/bookroom.db` 并删除旧的 `-wal`/`-shm`，再放回各目录。数据库与文件的时间点不一致时，库里有、磁盘上没有的书下载会 404，磁盘上多出来的文件无害。

## 反向代理

签名链接、OPDS 链接需要知道外部地址：设置 `BOOKROOM_PUBLIC_URL`，或让代理传 `X-Forwarded-Proto`/`X-Forwarded-Host`。上传大文件要放开代理的请求体上限，例如 nginx：

```nginx
server {
  server_name books.example.com;
  client_max_body_size 2g;
  location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_request_buffering off;
  }
}
```

## 元数据源

- 自动补缺只在精确匹配时写入：出版书按 ISBN（文件里有、或手动填写），网文（无 ISBN）按书名 + 作者在小说汇上完全一致。其余情况用 `POST /api/v1/books/{hash}/metadata/search` 查看候选、`…/apply` 手动应用。
- 某个源返回 429 或配额将尽时，它会被暂停到配额窗口滚动之后；连不上时暂停 10 分钟（`GET /api/v1/metadata/providers` 的 `pausedUntil`）。依赖它的任务改期等待，不计入重试次数，也不会被判为"无匹配"；暂停状态重启后仍然有效。
- 失败的任务指数退避重试，5 次后标记失败；`POST /api/v1/books/{hash}/metadata/refresh` 可重新排队。

## 观测

- `GET /healthz`：进程存活且数据库可用。
- 日志：每个请求一行（带 `X-Request-ID`），4xx 为 warn、5xx 为 error；健康检查不记录。
