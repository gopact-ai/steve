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

## Mailbox management

Steve 的「邮箱」页面（`#/mail`）通过 owner 鉴权的管理代理连接本 Provider。
先按前面的说明创建 `mail.received` provider 连接并保存连接凭据和加密密钥。
使用 Node.js 24+，在配置文件中设置 `accounts: []`，即可从页面添加账号；已有
配置账号也可作为首次启动的种子。管理后的账号、分类、标签、暂停状态和应用配置
保存在同一加密状态目录中，后续启动不会被旧配置覆盖。

生成一个至少 32 字符的独立管理凭据，在 **Steve 和 Provider 两个进程**的私有
环境中设置相同的 `STEVE_MAIL_ADMIN_TOKEN`。它与邮箱 provider 的连接凭据不同。
Steve 环境设置 `STEVE_MAIL_ADMIN_URL=http://127.0.0.1:8787`；Provider 配置可以
设置 `admin_port`，默认 8787，以及 `admin_token_env`，默认该环境变量名。

```sh
node src/cli.mjs serve --config config.json
```

重启 Steve 使代理环境生效后打开「邮箱」。管理服务仅监听 `127.0.0.1`，拒绝
浏览器 Origin 和缺少管理凭据的请求；页面通过已有的 owner 会话访问 Go 代理。
两个进程需要处于同一主机或通过已有的安全本地隧道连接。OAuth 的 Google 回调
仍是 Provider 所在主机的 loopback，首次 Gmail 授权应使用该主机的浏览器。
Microsoft 设备码可以在其他设备的官方登录页面完成。

页面提供分类导航、标签搜索、按分类/服务商折叠分组和状态筛选，邮箱列表在固定
高度区域内滚动，搜索与批量工具保持可用。可以多选后批量移动分类、暂停或恢复；
筛选变化会清除选择。若所选邮箱正在登录或同步，整批更新会被拒绝，避免部分更改。
分类名称最多 80 字符，每个邮箱最多 20 个、每个 40 字符的标签，最多管理 1,000
个邮箱。暂停保留登录与同步位置，恢复后继续检查；移除清除私有凭据并保留已发布
事件及去重身份。

### 导入与导出

支持 [1Password CSV](https://support.1password.com/export/)、
[1PUX v3](https://support.1password.com/1pux-format/) 和 Steve JSON，也可粘贴
邮箱地址列表。文件限 4 MiB，1PUX 解压后的数据限 8 MiB；只识别支持服务的邮箱
登录条目，跳过已归档、非邮箱与重复条目。1PUX 保险库名称作为分类，原有标签会
保留。CSV 使用标准 Tags，并在 Notes 中保存 Steve 分类信息，三个格式都可以
往返保留分类和标签。1PUX 导出按分类建立保险库。

导入前显示去除密码的预览，选择行后再写入本地加密账号库。预览保持 24 小时且
可在刷新后继续，提交使用事务和持久回执，重试不会重复创建账号。iCloud 必须
使用 Apple 应用专用密码；不能拿 Apple 主密码尝试登录。普通导入密码只有在
页面明确确认它是应用专用密码时才用于 iCloud 认证。

默认导出仅包含邮箱信息、分类和标签。勾选「包含已保存密码」才会导出导入保存的
密码或 iCloud 应用专用密码；OAuth 缓存、刷新令牌和应用密钥从不导出。CSV、
1PUX 和 JSON 均是未加密文件，包含密码的文件需私下保管并及时清理。这里导出
邮箱登录条目，不备份整个密码管理器的其他条目、附件、一次性密码或 passkeys。

### AI 自动导入与登录

在页面启用「AI 自动接入」，选择已有的私聊并按需要求逐次 Inbox 审批。该入口
创建独立的、仅限指定会话的 `client` 连接，授权七个 clients.v1 能力：
`mail.accounts`、`mail.import.preview`、`mail.import.commit`、`mail.login.start`、
`mail.login.status`、`mail.1password.status`、`mail.1password.preview`。关闭功能会撤销该连接。事件 provider 的授权保持独立。

先在页面上传文件并选择条目，再点击「通过 AI 导入并登录」。发给 AI 的指令
只含 client ID、预览 ID 和所选行 ID；文件、密码、OAuth 密钥及授权 URL/设备码
不会交给模型。AI 能读取脱敏元数据，提交已选择的账号，发起 SDK 登录任务并
读取实际回执。任务不占用长时间 claim，重放完成回执不会重复启动登录。
普通文件导入的 Google/Microsoft 同意、双重验证等步骤由用户在官方页面完成；设备码和
授权地址只在 owner 的邮箱页面显示。导入密码不能替代 OAuth 同意流程。

`serve` 内每个账号独立调度，慢账号不阻塞其他账号，手动同步与后台同步不会
重叠。页面显示真实的登录、同步中、失败、暂停与待发布数量；AI 返回的登录开始
回执不等于授权成功，以后续登录状态和同步结果为准。

### Agent 通过 1Password MCP 自动连接 Gmail

这条链路供 **Steve 的 agent** 使用，采用 MCP 官方 TypeScript SDK 提供 stdio
工具，复用 1Password 官方 CLI、Google 官方 SDK 和 Microsoft Playwright。
1Password 官方 [Environments MCP](https://www.1password.dev/environments/mcp-server)
只管理 Environment 和变量名称，不提供普通保险库 Login 条目的读取或 Gmail
登录工具，因此不能仅配置 `1password-mcp` 就把 Login 条目当作已登录邮箱。
Steve 的桥接只暴露邮箱操作，不向模型提供 `op item get`、密码或 OTP。

在安装 1Password 的主机上安装 [官方 CLI](https://www.1password.dev/cli/get-started)，
在桌面应用启用 [CLI 集成](https://www.1password.dev/cli/app-integration)，并保留
1Password 的解锁/系统认证提示。安装 Chrome。Provider 使用同一主机的本机
浏览器和 loopback 回调，不能在一台远程 Linux 机器上假装访问 Mac 的保险库。
`channel: "chromium"` 可改用经 `npx playwright install chromium` 安装的浏览器。

以 [config.onepassword.example.json](config.onepassword.example.json) 为起点：
`vaults` 是 owner 明确允许的保险库名称或 ID；多 1Password 账号时还可设置
`account` 为 CLI 账号标识。`command` 是 `op` 或官方 CLI 的绝对路径，不是 shell
片段。`auto_login: true` 开启本机自动填写；`approve_readonly_consent: true` 允许
在本次 SDK 发起的 Gmail **只读** OAuth 流程中点击官方同意按钮。关闭后在该
步骤等待本人。Provider 不创建 OAuth 应用、测试用户、密码或验证方式；Google
Desktop OAuth 应用仍需要一次配置，可以在现有邮箱页面导入其 JSON。

按上面的 `serve` 流程启动 Provider。再将
[mcp.node.example.json](mcp.node.example.json) 的定义合并到 **执行节点** 的 MCP
配置，替换 Node 24 和仓库的绝对路径。节点启动环境继承私有
`STEVE_MAIL_ADMIN_TOKEN`；这里的 token 与 Provider 相同，不写进 system prompt、
技能、项目文件或公开示例。标准 stdio 入口为：

```sh
node /absolute/path/to/steve/providers/mail/src/cli.mjs mcp
```

在需要使用该能力的 agent 上添加 `mcp_servers: ["mail_1password"]`，保留其已有
MCP 项，并将 [mail-connect](skills/mail-connect/SKILL.md) 目录加入 `skills`。
沿用 Steve 已有的节点 MCP 探测、broker 绑定和 agent 授权；无需修改核心 MCP
协议或把 Vault 密码送到 hub。节点本机的管理 token 限定这个桥接的 Provider。
通过 clients.v1 使用同样能力时，已有五项 grant 需关闭再启用 AI 接入，重新
授权七项能力。

之后可以直接对 Steve 说：**“把 vibe-coding 保险库里的 Gmail 导入并登录。”**
Agent 调用 `mail_1password_status` → `mail_1password_preview` →
`mail_import_commit` → 每个账号的 `mail_login_start` / `mail_login_status`。
每次调用带一个新 UUID `request_id`；丢失响应时用原 UUID 和相同参数重试，状态
轮询使用新 UUID。stdio MCP 和 clients.v1 只返回脱敏预览、账号 ID、阶段和真实
回执，没有凭据值、OAuth 地址/设备码或导出工具。

发现阶段只读取用户名；仅识别有 `accounts.google.com` 或 `mail.google.com`
HTTPS 网站的有效 Login 条目，Apple 等条目不会因用户名是 Gmail 被误导入。
提交保存加密的条目引用，不复制密码；登录时再次验证保险库、条目网站和邮箱
身份，再读取当前密码与临时 OTP。旧 refresh token、备注和 passkeys 不被导入。
邮箱导出也不包含 1Password 引用或运行时取得的密码。移除邮箱会清除引用。

多个 Gmail 的完整 OAuth 流程串行排队，排队时不提前启动 OAuth 超时；每次使用
独立的临时浏览器上下文，不使用用户 Chrome profile，不录制 trace/HAR/video。
常见用户名、密码、TOTP 和已启用的只读同意步骤自动执行。密码被拒后停止，不
反复尝试。Google 风控、验证码图片、硬件 passkey 或不支持的页面会报告
`needs_owner`，并在现有登录窗口等待；不承诺绕过 Google 的自动化检测。
队列、自动填写、等待本人和授权成功在邮箱页面分别提示。只有 Google SDK
完成 PKCE/state、离线令牌和实际邮箱身份验证后才写入 `authorized`。

此版 1Password 自动浏览器登录支持 Gmail。Outlook/Hotmail 和 iCloud 继续使用
前述设备码与 Apple 应用专用密码流程，不能将 Apple 主密码当作 IMAP 凭据。

## 验证

```sh
npx playwright install chromium
npm test
STEVE_MAIL_INTEGRATION=1 npm test
```

也可设置 `BROWSER_CHANNEL=chrome`，使用已安装 Chrome 运行隔离浏览器测试。

测试覆盖分页、过期游标恢复、跨账号去重、正文截断、缓存加密与身份绑定、发布
回执丢失和容量回滚，并覆盖真实导出格式解析、分类往返、50 邮箱批量原子更新、
管理鉴权和不含密码的 AI 能力回执。`make test-console` 的邮箱验收还通过真实
Go HTTP 服务检查预览刷新、登录任务、导出、分类/筛选、固定区域滚动、键盘和
窄屏恢复。Google/Graph SDK 对隔离 HTTP 服务器的实际调用、ImapFlow
的真实 EXAMINE/BODY.PEEK 协议请求也被验证。集成测试构建已有的 Steve fixture，
使用真实 HTTP、连接鉴权与 ledger 验证受理、重复提交、注册、心跳与撤销。
这些测试使用隔离邮件与授权替身；真实邮箱登录和真实模型摘要需要配置账号后
另行验收，不在自动测试中冒充完成。

新增测试用真实 stdio MCP 客户端验证发现、提交、权限失败和持久重放；官方 CLI
替身验证保险库限定、Apple/归档排除、密码仅在登录时读取、身份变化拒绝及错误
脱敏。隔离 Chrome 对受控登录页验证用户名/密码/OTP 自动填写、跨源拒绝、PKCE
loopback 回调和串行窗口。这些测试不访问用户的真实 1Password 或 Gmail。
