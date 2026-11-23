# 客户端硬分叉 Readest，删掉套餐概念而不是绕过门控

客户端以 Readest v0.12.20 为基础硬分叉，直接删除官方商业和云服务代码。`hirofumo/readest-unlocked` 的做法是构建时打补丁，把 `utils/access.ts` 的 `isCustomizationAllowed` 强制改成放行，死代码仍留在包里；我们直接删掉套餐和配额（`UserPlan`、`types/quota.ts`、`utils/access.ts` 里的门控），原来被门控的功能（TTS 按章下载、自定义翻译器、网盘同步等）因为门控代码不存在而全部可用。

## 删除

- 支付订阅：`libs/payment/`（Stripe、Apple/Google 内购）、`app/user/`、`app/api/stripe`、`types/payment.ts`、`hooks/useQuotaStats.ts`。
- 账号与官方后端：Supabase 全套、`app/auth`、Apple 登录插件，以及 `src/app/api/*` 和 `src/pages/api/*` 的官方后端路由（由 BookRoom 取代）。
- 官方云服务：分享链接（`app/s`、`libs/share*`）、邮件投递到 Readest、Readest Voice、服务端翻译配额路由（deepl、azure、yandex）、CrossPoint 设备绑定、统计归档到 R2、`libs/workersWebSocket.ts`。
- 部署与遥测：Cloudflare/OpenNext/wrangler、PostHog、Sentry；自动更新改为指向自己的发布地址。
- 用不到的集成：Audiobookshelf 客户端与有声书播放器（连同 BookOrbit 连接器里的有声书部分）、RSS 订阅、网页小说导入、URL 剪藏。

## 保留

AI 助手（自带 OpenAI 兼容 key）、笔记导出（Notion、Obsidian、Readwise）、网盘同步（WebDAV、GDrive、S3、OneDrive、iCloud）、OPDS 客户端、KOSync 客户端、Grimmory 与 BookOrbit 连接器（作为远程，见 ADR 0010）、Hardcover 进度同步、BookDrop/LocalSend 局域网传书、翻译、词典、TTS。

## 考虑过的方案

- 补丁配方（readest-unlocked 式）：每天自动跟进上游、差异小，但死代码留在包里，删得越多补丁越容易失效。

## 后果

- 上游的阅读器改进需要手动合并。我们删掉的文件出冲突时直接保留删除即可；真正要逐个处理的是原门控调用点（约 30 个文件）。
- TXT 转 EPUB 在 `utils/txt.ts`（`TxtToEpubConverter`），不受删除影响，只用于打开时显示（ADR 0011）。`services/send/conversion` 负责 DOCX/RTF 入库时转 EPUB（`bookService.ts:639-675`），删邮件投递时这个目录要保留。
- Web 版浏览器有 CORS 限制，连第三方服务要经服务端代理：`app/api/opds/proxy`、`pages/api/kosync.ts`、`pages/api/bookorbit.ts`、`pages/api/grimmory.ts`、`app/api/hardcover/graphql`、`app/api/notion`。Tauri 版走 `@tauri-apps/plugin-http`，不需要这些代理。Web 版静态导出后跑不了 Next 路由，所以由 BookRoom 服务端提供这些代理，并照搬 `opds/proxy` 中每次重定向都重新检查的 SSRF 防护。
- 客户端是 AGPL-3.0，分发时必须提供源码。
