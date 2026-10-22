# 邮件 Provider

将 Gmail、Outlook、Hotmail 和 iCloud Inbox 的新邮件送入 Steve，供你查看或交给 agent 摘要和整理待办。支持多个账号、分类、标签及独立同步。新增账号不会自动创建订阅或启动 agent。

Provider 读取邮件，不发送回复、不改标签或已读状态，也不下载附件正文。邮件正文作为外部资料交给 Steve，最大 48 KiB UTF-8，超限以 `body_truncated` 标明。

## 安装与连接

使用 Node.js 24+，可运行在 macOS 或 Linux：

```sh
cd providers/mail
npm ci
cp config.example.json config.json
```

编辑[配置](config.example.json)，保留需要的账号并设置主登录邮箱。同一服务商可配置多个账号；`id` 是持久身份，更换邮箱或服务商使用新 ID。状态目录和 Google 凭据文件路径相对于配置文件解析。

在 Steve 连接页创建允许 `mail.received` 的专用 Provider 连接，把一次性凭据保存到 `STEVE_MAIL_CONNECTION_TOKEN`。`steve_url` 使用 HTTPS 或 SSH 隧道的本地端口；不要使用 owner token 替代连接凭据。

生成 32 字节加密密钥，例如用 `openssl rand -base64 32`，将其保存在私有服务环境的 `STEVE_MAIL_SECRETS_KEY`。重启必须保留同一密钥，配置文件只引用环境变量名。

## 账号授权

| 邮箱 | 连接和授权 |
| --- | --- |
| Gmail | Google Gmail SDK；Desktop app OAuth、PKCE 与本机浏览器 |
| Outlook / Hotmail | Microsoft Graph 与 MSAL；个人账号设备码授权 |
| iCloud | ImapFlow 与 MailParser；IMAP/TLS 和 Apple 应用专用密码 |

### Gmail

在 Google Cloud 启用 Gmail API，创建 Desktop app OAuth 客户端，将下载的 JSON 放到 `credentials_file`。测试应用需将邮箱加入测试用户。申请的邮箱权限为 `gmail.readonly`。

```sh
node src/cli.mjs login --config config.json --account personal-gmail
```

授权在 Provider 所在机器的浏览器完成，回调只监听 loopback 并验证 state。SDK 核对邮箱身份后保存刷新凭据。授权撤回或过期时重新登录。参考 [Google 设置指南](https://developers.google.com/workspace/gmail/api/quickstart/nodejs)。

### Outlook / Hotmail

注册支持 Personal Microsoft accounts 的 Microsoft 应用，并启用 Allow public client flows。一个 `client_id` 可以授权多个账号，每个账号有独立缓存。权限为 delegated `Mail.Read`、`User.Read`。

```sh
node src/cli.mjs login --config config.json --account personal-outlook
```

按官方设备码页面完成授权。账号配置使用主登录地址，别名不是独立邮箱。后台运行不弹出新的登录；需要重新授权时停止 Provider，再运行登录命令。

### iCloud

启用 Apple Account 双重认证并创建[应用专用密码](https://support.apple.com/en-us/102654)，保存到 `password_env` 指定的环境变量。默认连接 `imap.mail.me.com:993`，username 默认完整邮箱地址。

```sh
node src/cli.mjs login --config config.json --account personal-icloud
```

该命令验证认证和 Inbox 读取，不使用 Apple 主密码。服务器设置见 [Apple IMAP 文档](https://support.apple.com/en-us/102525)。

## 同步与订阅

```sh
node src/cli.mjs once --config config.json
node src/cli.mjs run --config config.json
node src/cli.mjs status --config config.json
```

`once` 检查一轮，任意账号失败时退出码为 1。`run` 持续同步，默认每 60 秒检查，各账号互不阻塞。Provider 每 15 秒发送心跳；进程在线与邮箱登录成功分别查看，`status` 可与后台进程同时使用。

首次同步以当时 Inbox 为基线，不把历史邮件全部发送给 agent。Gmail 使用 history ID，Microsoft 使用 delta，iCloud 使用 UID/UIDVALIDITY。游标失效时从首次同步时间扫描仍在 Inbox 的内容并去重；已移动或删除的邮件不保证被观察到。

在 Steve 创建 `mail.received` 的 Work 订阅，选择已绑定项目的会话，例如：

> 概括邮件内容，列出明确的待办、截止时间和需要回复的事项。

事件包含账号、服务商、发件人、收件人、主题、时间、正文及附件存在标记。多个邮箱共用一个来源连接。订阅按来源与事件类型匹配，没有任意 payload 筛选；指令可引用账号字段。

## 控制台邮箱管理

「邮箱」页通过 owner 管理代理连接 Provider。可用 `accounts: []` 从页面添加账号；已有配置可作为首次启动种子，后续管理状态保存在私有状态目录，不被旧配置覆盖。

在 Steve 与 Provider 的私有环境中配置相同的、至少 32 字符的 `STEVE_MAIL_ADMIN_TOKEN`。Steve 设置 `STEVE_MAIL_ADMIN_URL=http://127.0.0.1:8787`；Provider 的 `admin_port` 默认 8787，`admin_token_env` 默认上述变量名。

```sh
node src/cli.mjs serve --config config.json
```

重启 Steve 使代理环境生效。管理服务仅监听 loopback，两个进程需在同机或通过安全本地隧道连接。Gmail 回调仍需 Provider 所在机器的浏览器；Microsoft 设备码可在其他设备完成。

页面支持分类、标签、搜索、状态筛选和批量移动、暂停、恢复。暂停保留同步位置，移除清理私有凭据并保留已发布事件及去重身份。正在登录或同步的账号会阻止整批更新。

最多管理 1,000 个邮箱；分类名 80 字符，每邮箱最多 20 个标签、每标签 40 字符。

### 导入和导出

支持 Steve JSON、邮箱地址列表，以及 1Password CSV 和 1PUX v3 文件格式。这是文件导入，不连接密码管理器。文件上限 4 MiB，1PUX 解压数据上限 8 MiB，仅识别支持服务商的邮箱条目。

导入先显示不含密码的预览，选择条目后提交；预览保留 24 小时，重试沿用持久回执。分类和标签可以往返保留。iCloud 密码只有在明确确认为应用专用密码时用于认证。

默认导出不含密码。显式选择后可导出保存的登录密码或应用专用密码，但不导出 OAuth 缓存、刷新令牌和应用密钥。导出文件未加密，也不是完整密码管理器备份。

### Agent 协助接入

「AI 自动接入」为选定私聊创建独立客户端授权，可要求逐次 Inbox 审批。支持账号元数据、导入预览与提交、登录启动和状态查询。

你先上传文件并选择条目，再请求 agent 导入和登录。模型只收到 client、预览和行 ID，不收到文件、密码、OAuth 密钥、设备码或授权 URL。Google/Microsoft 的同意及双重验证由你在官方页面完成。

登录开始的回执不代表授权成功，页面和查询接口显示后续状态。关闭 AI 接入撤销客户端连接，不改变邮件事件 Provider 授权。

## 状态与恢复

SQLite 原子保存同步位置和待发布事件；受理响应丢失时按原 key 重试。收到 Steve 回执后清除本地待发布正文，保留去重身份。待发布上限 1,000 条，到达上限时不推进同步位置。

状态目录权限为 0700，数据库为 0600。Google/MSAL 缓存使用 AES-GCM 加密；同步位置、邮箱身份和待发布邮件仍是私有业务数据，需连同密钥妥善备份。状态随去重记录增长。

一个状态目录由一个同步或登录进程持有。更换 Steve 凭据时旋转原连接；不同连接使用独立状态目录。同步成功仅表示事件发布，agent 结果在 Steve 的原会话和 Work 记录中确认。

开发验证使用 `npm test`，`STEVE_MAIL_INTEGRATION=1 npm test` 包含隔离 Steve 服务与 SDK 协议检查。自动验证不代替真实账号授权和真实邮件处理验收。协议详见[事件与客户端接口](../../docs/event-providers.md)。
