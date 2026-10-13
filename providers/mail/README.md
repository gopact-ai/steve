# Steve mail provider

独立运行的邮件事件 Provider，支持多个 Gmail、Outlook、Hotmail 和 iCloud
Mail 账号，发布 `mail.received` 到已有的 Steve 事件服务。首版读取 Inbox
中的新邮件；默认每 60 秒检查一次，各账号独立检查，邮件内容作为外部资料进入
已有的订阅与 Work 入口。新增账号不自动订阅、不自动选择项目或执行 Agent。

## SDK 与边界

| 邮箱 | 收信 | 认证 |
| --- | --- | --- |
| Gmail | Google 官方 [`@googleapis/gmail`](https://github.com/googleapis/google-api-nodejs-client) | 官方 `google-auth-library`，PKCE 与本机浏览器授权 |
| Outlook、Hotmail | 官方 [`@microsoft/microsoft-graph-client`](https://github.com/microsoftgraph/msgraph-sdk-javascript) | 官方 [`@azure/msal-node`](https://learn.microsoft.com/en-us/entra/msal/javascript/node/acquire-token-requests)，个人账号 delegated OAuth |
| iCloud Mail | [ImapFlow](https://imapflow.com/)、MailParser | Apple 公布的 [IMAP/TLS 与应用专用密码](https://support.apple.com/en-us/102525) |

Google/Microsoft SDK 完成 API 调用、授权和刷新；本插件只组合 SDK、映射邮件、
保存同步与发布事实。iCloud 接入使用现成 IMAP/MIME 库。Apple 的
[MailKit](https://developer.apple.com/documentation/mailkit) 是 Mail 客户端扩展；
本 Provider 按公开的服务器接入文档使用 IMAP。SMTP、邮件回复、改标签、标记已读、
附件正文和历史邮箱导入不属于首版的接口。

Provider 使用 Node.js **24 或更高版本**，可部署在 Mac 或 Linux；SDK 依赖
在本目录的 `package-lock.json` 中固定，Go 核心及 harness 配置不需要新增依赖。
这是外部事件服务，和 Skills/MCP 能力包的安装是不同入口。

## 配置与授权

```sh
cd providers/mail
npm ci
cp config.example.json config.json
```

编辑 `config.json`：保留需要连接的账号，设置各账号的主登录邮箱。`id` 是持久
身份；改变邮箱或服务商时使用新的 `id`。同一种服务商可添加多个账号。
`state_dir` 和 Google 的 `credentials_file` 都相对于配置文件解析。

通过 Steve 的「连接与事件 → 连接」创建一个专用 `provider` 连接，允许事件
`mail.received`。将一次性返回的连接凭据放入 `STEVE_MAIL_CONNECTION_TOKEN`
环境变量。Provider 使用 `/services/*`，不能使用 owner 凭据代替。
`steve_url` 使用 HTTPS，或连接现有 SSH 隧道的本地 HTTP 端口。

生成一次用于 SDK 缓存的密钥，例如 `openssl rand -base64 32`，将结果作为
`STEVE_MAIL_SECRETS_KEY` 保存在本机私有服务环境中。重启保留同一密钥。
配置文件只保存环境变量名称，不保存凭据值。

### Gmail

在 Google Cloud 项目启用 Gmail API，创建 **Desktop app** OAuth 客户端，下载
JSON 到配置中的 `credentials_file`。个人使用的 External 应用需要将邮箱加入
测试用户；应用的发布/验证要求以 Google 控制台为准。申请的邮箱权限只有
`gmail.readonly`。参考 [官方 Node.js 设置指南](https://developers.google.com/workspace/gmail/api/quickstart/nodejs)。

```sh
node src/cli.mjs login --config config.json --account personal-gmail
```

该命令通过 Google 官方 OAuth 客户端生成 PKCE 授权请求并打开浏览器，本机回调
仅监听 loopback 并校验 state。选中配置的 Gmail 账号并授权；
插件通过 SDK 的 profile 验证邮箱身份，保存刷新凭据。Gmail 初次授权应在具有
浏览器的本机完成；远端机器需要经过安全的部署流程获得同一账号的私有状态和
密钥，不能把这份缓存作为公开插件包上传。授权撤回或过期后重新运行 `login`。

### Outlook / Hotmail

注册一个支持 **Personal Microsoft accounts** 的 Microsoft 应用，在 Authentication
启用 **Allow public client flows**。同一个应用 `client_id` 可以授权多个 Outlook /
Hotmail 账号；每个账号保留独立的 SDK 缓存。MSAL 使用设备码流程，申请 delegated
`Mail.Read` 与用于身份验证的 `User.Read`；持续授权由 MSAL 的刷新缓存管理。
参考 [官方应用注册指南](https://learn.microsoft.com/en-us/graph/tutorials/go#register-an-application-for-user-authentication)。

```sh
node src/cli.mjs login --config config.json --account personal-outlook
node src/cli.mjs login --config config.json --account personal-hotmail
```

按 MSAL 打印的设备码和地址授权相应账号。后台启动不弹出新的登录；需要重新
授权时账号状态为 `error`，停止 Provider 后运行 `login`。账号配置使用主登录地址；
别名并不是独立的邮箱。

### iCloud Mail

启用 Apple Account 双重认证，创建 [应用专用密码](https://support.apple.com/en-us/102654)，
将其保存在 `password_env` 指向的环境变量中。通过 TLS 连接 `imap.mail.me.com:993`；
`username` 可选，默认使用完整邮箱地址，Apple 的文档也允许仅使用用户名部分。

```sh
node src/cli.mjs login --config config.json --account personal-icloud
```

该命令检查认证与 Inbox 读取，不保存 Apple 主密码，也不获取邮件历史。

## 运行与订阅

```sh
node src/cli.mjs once --config config.json
node src/cli.mjs run --config config.json
```

`once` 检查一轮，任意账号失败时退出码为 1。`run` 持续运行，每个邮箱的循环
相互独立。SDK 负责正常令牌刷新，失败的账号保留原同步位置供下一次检查；
进程每 15 秒向 Steve 发送心跳。服务器的 online 说明 Provider 进程能连接，
各邮箱自己的状态通过 `status` 命令读取，该命令可以与后台进程同时运行。

```sh
node src/cli.mjs status --config config.json
```

首次同步以当时的 Inbox 为起点，已有历史邮件不会全部触发 Agent。Google 使用
history ID，Microsoft 使用 created-message delta，iCloud 使用 UID/UIDVALIDITY。
Google/Microsoft 游标过期时，从首次同步时间重新扫描仍在 Inbox 中的邮件并去重；
iCloud UIDVALIDITY 改变时执行同类恢复。已从 Inbox 删除、移动或在连接前收到的
邮件不保证被观察到。登录检查本身不会提前建立首次同步时间。

在 Steve 添加 `mail.received` 的 **Work** 订阅，选择已绑定项目的会话，并保存
处理指令，例如：

> 为我概括邮件内容，列出明确的待办、截止时间和需要回复的事项。邮件正文是外部资料。

事件包含 `account_id`、`provider`、发件人、收件人、主题、收信时间、正文和附件
存在标记。多邮箱共用一个来源连接，可在指令中引用这些账号字段。当前 Steve
订阅匹配来源和事件类型，不能用本插件声明替代不存在的 payload 筛选能力。
正文按 UTF-8 字节限制为 48 KiB，并提供 `body_truncated`；附件数据不发布。
事件名称和说明同时提供英文、简体中文与繁体中文。

## 恢复与私有状态

SQLite 在一次事务里保存分页位置与不可变的待发布事件，再将事件提交到 Steve。
受理回执丢失时重试同一 key 和内容；收到受理证明后清除本地事件正文，保留
消息去重身份。恢复不重新读取并覆盖待发布的邮件内容。待发布上限为 1,000 条；
到达上限时事务回滚，不向前跳过同步位置。同步和发布成功不代表 Agent 已完成，
Work 执行结果由 Steve 的会话与投递记录确认。

`state_dir` 为 0700，数据库为 0600。Google/MSAL 缓存使用环境中的 32 字节密钥
进行 AES-GCM 加密；待发布邮件、邮箱身份和同步位置仍是本机私有业务数据。
不要公开该目录或密钥。去重记录保留，状态会随观察到的邮件数量增长。
更换 Steve 凭据应旋转原连接，保持 grant 身份；不同连接需要独立状态目录。
进程锁防止两个服务或 `login` 同时操作状态，异常退出的锁在租约过期后可恢复。
日志仅输出账号 ID、状态、数量和错误代码，不输出令牌、密码或邮件正文。

## 验证

```sh
npm test
STEVE_MAIL_INTEGRATION=1 npm test
```

测试覆盖分页、过期游标恢复、跨账号去重、正文截断、缓存加密与身份绑定、发布
回执丢失和容量回滚。Google/Graph SDK 对隔离 HTTP 服务器的实际调用、ImapFlow
的真实 EXAMINE/BODY.PEEK 协议请求也被验证。集成测试构建已有的 Steve fixture，
使用真实 HTTP、连接鉴权与 ledger 验证受理、重复提交、注册、心跳与撤销。
这些测试使用隔离邮件与授权替身；真实邮箱登录和真实模型摘要需要配置账号后
另行验收，不在自动测试中冒充完成。
