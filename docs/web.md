# Web 镜像

Web 服务是 CLI 的**实时网页镜像**：它与 CLI 共享同一个 Agent、同一份会话，
CLI 与网页（可同时多个）都能输入；Agent 的事件通过 **WebSocket** 双向实时同步。

## 启用

在 `config.json` 中设置：

```jsonc
"web": { "host": "127.0.0.1", "port": 8080, "password": "secret", "password_salt": "<32 位十六进制>" }
```

* `port > 0` 即启用。命令行可用 `--web-port N` 临时覆盖、`--no-web` 直接关闭。
* `host` 为监听地址，默认（空/省略）`127.0.0.1` 仅本机；设为 `0.0.0.0` 可对局域网开放（请配合 `password`）。
  命令行可用 `--web-host IP` 覆盖。
* 端口被占用时自动尝试 `port+1`、`port+2` …（最多 +50）；启动横幅会打印实际端口。
* `password` 非空即要求**登录**；`password_salt` 会由程序自动生成（手写 `password` 后启动即可），
  详见[登录与鉴权](#登录与鉴权)。

访问 `http://<host>:<port>/`（默认 `http://127.0.0.1:<port>/`）。

## 登录与鉴权

登录是**页面里的对话框**（不再使用浏览器 Basic 弹窗）。密码本身**不出浏览器**：

1. 页面向 `GET /api/session` 询问「是否需要登录」以及**公开的盐**（`web.password_salt`）；
2. 用户在对话框里输入密码，浏览器用 `sha256(盐 + 密码)` 算出**加盐摘要**，只把摘要发给 `POST /api/login`；
3. 服务端用配置文件里的明文密码算出同样的摘要并比较（常量时间），成功后下发 **HttpOnly 会话 cookie**；
4. 之后页面、`/api/config`、`/api/password` 与 **WebSocket 握手**都靠这个 cookie 通过（没有 `?token=` 之类的东西了）。

* **浏览器保存（记住我）**：勾选 “Stay signed in on this browser” 后
  * 会话 cookie 带 `Max-Age`（30 天，默认 12 小时但关掉浏览器即失效），因此重开浏览器仍然登录；
  * 页面把**加盐摘要**（不是密码）存进 `localStorage`，cookie 丢失时自动静默登录；
    换密码时盐会轮换，旧摘要随即失效并被丢弃（不会重放）。
  * 对话框是标准 `<form>`（`autocomplete="current-password"` + 固定的用户名字段），
    浏览器自带的密码管理器也能保存密码；退出登录（页头 **Sign out**）会清掉会话与本地摘要。
* **设置/更换密码**：配置编辑器 `web.password` 那一行有 **Set / Remove** 按钮（`POST /api/password`）：
  服务端生成新盐、写入 `config.json` 并**立即生效**（无需重启），其它浏览器的会话会被登出，当前浏览器保持登录。
  也可以直接编辑 `config.json`（写 `password`，程序下次启动自动补 `password_salt`）。
* 防护：连续 5 次错误摘要后按地址锁定（5s、10s…最多 5 分钟，返回 `429`）；写操作（`/api/login`、`/api/logout`、
  `/api/config`、`/api/password`）要求同源（`Origin`/`Sec-Fetch-Site` 校验）。
* 页面与静态资源（`/`、`*.css`、`*.js`、`/assets/`）**公开**（不含任何数据，且登录对话框必须先能加载）；
  `/ws`、`/api/config`、`/api/password`、`/api/logout` 都要求会话；`/api/session`、`/api/login` 匿名可访问。

> **安全边界（务必了解）**：服务端按需求在 `config.json` 里**明文保存**密码（摘要由它推导，因此必须持有明文）。
> 摘要是「把密码换成加盐哈希」的传输/存储形态，它**等价于该服务的口令**：在明文 HTTP 下被截获即可重放，
> 所以对局域网开放时请配合反向代理的 TLS，或只在 `127.0.0.1` 上使用。浏览器保存的摘要同理，
> 仅对**本机配置**有效（换密码即失效，换机器无用）。

## HTTP 端点

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/` | 单页 UI（服务端注入 markdown 开关），公开 |
| GET | `/ws` | WebSocket 升级端点（**需要会话**） |
| GET | `/api/session` | 是否需要登录、当前是否已登录、公开盐，公开 |
| POST | `/api/login` | 提交加盐摘要换取会话 cookie（`{"digest":"…","remember":bool}`），公开但受限流 |
| POST | `/api/logout` | 结束当前会话（**需要会话**） |
| GET | `/api/config` | 读取 `config.json`（`api_key`/密码打码，见下节）（**需要会话**） |
| PUT | `/api/config` | 校验并写回 `config.json`（**重启后生效**）（**需要会话**） |
| POST | `/api/password` | 设置/清除登录密码（**立即生效**）（**需要会话**） |
| GET | `/app.css` | 页面样式（内嵌），公开 |
| GET | `/app.js` | 页面脚本（内嵌），公开 |
| GET | `/auth.js` | 登录对话框脚本（内嵌），公开 |
| GET | `/config.js` | 配置编辑器脚本（内嵌），公开 |
| GET | `/assets/marked.min.js` | 内嵌的 Markdown 渲染库（marked，MIT） |
| GET | `/assets/dompurify.min.js` | 内嵌的 HTML 净化库（DOMPurify，Apache-2.0/MPL-2.0） |

## 页面文件

前端源码以**普通文件**内嵌（`internal/web/`，`go:embed`），不引入构建步骤、不依赖 CDN：

| 文件 | 说明 |
|------|------|
| `index.html` | 页面骨架（含 viewport、登录对话框与运行时 markdown 开关占位符），服务端每次请求替换后再返回 |
| `app.css` | 页面样式（响应式布局与主题） |
| `app.js` | 页面行为（WebSocket、渲染、输入） |
| `auth.js` | 登录对话框：取盐、算加盐摘要（自带 SHA-256，明文 HTTP 下没有 WebCrypto）、保存摘要、连接门控 |
| `config.js` | 配置编辑器（表单 / JSON 两种模式，访问 `/api/config`、`/api/password`） |
| `assets/` | 第三方库（marked / DOMPurify），见该目录的 `README.md` |

`app.css`、`app.js`、`auth.js`、`config.js` 由 `/app.css`、`/app.js`、`/auth.js`、`/config.js` 提供，和页面、
`/assets/` 一样**公开**（不含数据）；受保护的只有 `/ws` 与 `/api/*` 中的数据端点，它们靠会话 cookie，
因此子请求不再需要 `?token=`。

页面与内嵌资源（`/`、`*.css`、`*.js`、`/assets/`）都返回 `Cache-Control: no-cache`：
UI 随二进制内嵌，重建后浏览器会重新校验，不会继续使用旧版脚本/样式。

## 配置编辑（`/api/config`）

**入口**：页头右上角的 **⚙**（手机端同样可见），或桌面端侧栏的 “Configuration” 卡片。
面板里对 `config.json` 提供两种编辑模式，右上角的 **Form / JSON** 标签切换：

* **Form（表单）**：按段展开的控件表单，每项一行——左侧是配置键名 + 说明，右侧是控件：
  文本框、密码框、数字框、开关、滑杆（带实时数值）、下拉、多行文本。
  段落为 OpenAI / Context / Web mirror / Tools · shell / Tools · files / Tools · discovery /
  Tools · MCP / Agent / UI（`▸` 可折叠，OpenAI、Context、Web 默认展开）。
  控件改动**立刻写进当前文档**，所以切到 JSON 看到的就是将要提交的内容。
  留空表示“用内置默认”（保存时该项直接从文件中移除）；`extra_body` 与 `tools.mcp.servers`
  是这两个 map 型字段的 JSON 文本框（标有 `json` 标签）。
* **JSON**：整份文档原文编辑，适合复制粘贴或表单没覆盖的场景。切回表单时以文本框内容为准；
  若 JSON 非法，则**留在 JSON 模式**并提示错误，不会丢掉已输入的内容。

**保存前会在两端校验**：

* 客户端：数字项非数字/超范围、JSON 文本框解析失败 → 行内红字 + 该行高亮，**Save 被阻止**并提示
  是哪个字段；
* 服务端：未知字段（拼写错误）、缺少 `openai.api_key`、启用的 MCP server 缺 `command`/`url` 等 → `400`，
  状态行显示服务端原文（未知字段会额外提示去 JSON 模式删除）。

成功保存后状态行显示 `saved — restart lightagent to apply it`。**保存只写文件，运行中的进程继续
使用启动时的配置**，重启 lightagent 后才生效（面板始终提示这一点）。

### 接口

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/config` | 读取当前文件（默认值补全、`api_key` 打码） |
| PUT | `/api/config` | 请求体为 JSON 文档；校验后写回文件（也接受 `POST`，便于 curl） |

两者都在与页面相同的鉴权之下（需要登录后的会话 cookie）。响应结构（GET 与 PUT 相同）：

```json
{ "path": "D:\\...\\config.json", "config": { ... }, "pending_restart": false }
```

* 返回的文档已按**启动时的规则**合并默认值（缺失字段显示默认值、`tools.mcp.servers` 会补上占位 server），
  且 `openai.api_key` 与 `web.password` **打码为 `***`**（与 `--print-config` 一致）。保存时若这两个字段为 `***` 或空，
  服务端沿用文件里原有的值，因此改模型等其它字段无需重新输入；要换密钥/密码直接粘贴新值即可
  （新密码会**立即生效**并轮换盐，见[登录与鉴权](#登录与鉴权)）。`web.password_salt` 是公开值，按原样回显。
* `web.password` 那一行是**密码控件**（`Set` / `Remove` + 状态行），走 `POST /api/password`：
  写入文件并立即生效，不用重启；其余字段仍是「保存后重启生效」。
* 被拒绝时**文件原样保留**；写回的文件一定可以通过启动校验。
* **Reload** 重新从磁盘读取（外部编辑也能显示）；未保存的改动在重新打开面板时仍保留。
* 会话过期时任何请求都会返回 `401`，面板会提示重新登录（页面自动弹出登录对话框）。
* 命令行临时覆盖（`--model`、`--api-base`、`--web-port` 等）只作用于当前进程，不会写回文件。
* 该接口仅在镜像启动时接入了配置文件（正常启动总是如此）时可用，否则返回 `404`。

## WebSocket 协议

连接 `ws://127.0.0.1:<port>/ws`（需要登录后的会话 cookie，浏览器会自动带上；未登录握手会被拒绝）。
所有数据帧均为 UTF-8 文本，内容为 JSON。

### 服务端 → 客户端

**1. 首次连接时推送历史**（随后才是实时事件）：

```json
{ "type": "history", "summary": "可选摘要", "tokens": 1234, "window": 131072, "messages": [ { "role": "user", "content": "..." } ] }
```

> 页面收到 `history` 时会**清空并重建**整个日志区（历史始终是全量快照），因此断线重连
> 不会把同一条消息再显示一遍。`tokens`/`window` 用于顶部上下文用量徽标。
>
> `messages[].role` 取值与页面绘制的行一一对应：`user`、`assistant`、`reasoning`（模型
> 思考，斜体灰字）、`tool_call`、`tool_result`、`info`、`error`、`interrupted`。
>
> 服务端在启动时就初始化自己的**内存回放缓冲**：先用当时的会话（如已恢复的历史）播种，
> 之后把事件总线上的每个事件追加进去（思考增量会合并成一条 `reasoning` 行）。因此网页
> 之后才打开、或刷新重连，都能看到完整的对话，包括思考、工具行与 info/error 标记，
> 且不受上下文压缩影响（与终端回滚一致）。

**2. 实时事件**：字段与 Agent 事件一致。

```json
{ "type": "reasoning_delta", "text": "模型思考增量" }
{ "type": "assistant_delta", "text": "部分文本" }
{ "type": "assistant", "text": "完整回复" }
{ "type": "tool_call", "name": "exec_command", "args": "{\"command\":\"ls\"}" }
{ "type": "tool_result", "name": "exec_command", "text": "展示文本", "is_error": false }
{ "type": "info", "text": "..." }
{ "type": "compacted", "text": "context compressed: 20 -> 9 messages" }
{ "type": "interrupted", "text": "interrupted; the turn was stopped" }
{ "type": "error", "text": "..." }
{ "type": "usage", "tokens": 1234, "context_window": 131072 }
{ "type": "user", "text": "某客户端发送的消息", "source": "web" }
{ "type": "turn_done" }
```

* `usage` 在上下文增长的每个时点广播（回合开始、每次模型回复、每轮工具执行后），
  页面据此**实时**更新顶部上下文用量徽标（`tokens` 以接口返回的 `prompt_tokens` 为基准）。
* `user` 事件新增 `source` 字段（`cli` / `web`）：终端已回显本地输入，因此 CLI 只渲染
  其它来源（如网页）的 `user` 事件，网页输入因此能在 CLI 中看到。
* `reasoning_delta` 携带服务商暴露的模型思考（`reasoning_content` / `reasoning`）增量；
  页面用一个灰色 thinking 行流式渲染（每 120ms 重绘），收到后续非思考事件时定稿。
  `ui.markdown=true` 时该行与回答一样经 marked + DOMPurify 渲染为 Markdown，否则显示斜体纯文本。
  组装后的思考写入 assistant 消息（并以 `reasoning_content` 回传模型），同时进入服务端的
  内存回放缓冲，因此刷新/重连后的 `history` 帧也会以 `reasoning` 行回放思考（CLI 的
  `/history` 仍只显示可见回答）。

> **Markdown 渲染在浏览器端完成**（`ui.markdown=true` 时）：页面加载内嵌的
> [marked](https://github.com/markedjs/marked)（GFM，支持表格）把消息渲染为 HTML（回答与
> 思考行一致），再用 [DOMPurify](https://github.com/cure53/DOMPurify) 净化后才写入
> `innerHTML`，因此不受服务端字段影响，协议保持与 Agent 事件一致。流式阶段每 120ms 重渲染
> 一次当前回复，收到最终 `assistant` 时用完整文本定稿（同一行覆盖，不会重复出现两条回复）；
> `ui.markdown=false` 时不再调用库，直接用 `textContent` 显示纯文本。

> 说明：`user` 事件会广播给所有客户端（包括发送者），因此页面无需本地回显；
> CLI 通过 `source` 字段区分来源，仅渲染非本终端（如网页）的输入。

### 客户端 → 服务端

发送一条用户消息：

```json
{ "text": "帮我看看当前目录" }
```

服务端会对 `/result [on|off]`（开关工具结果输出，与 CLI 共享）和 `/stop`（中断当前回合）
做本地处理，其余文本调用 `Agent.SubmitFrom("web", ...)`：
* Agent 空闲 → 开启新回合；
* Agent 忙碌 → 作为 steering 插入当前回合。

发送时机不限制：任意客户端可在任意时刻发送，包括其它客户端/ CLI 正在运行回合时。

## 输入与显示

* 输入框：**Enter 换行、Ctrl+Enter 发送**（Shift+Enter 也是换行，可粘贴多行文本）；
  随内容自动增高（最多约 40% 视口高度，超出后内部滚动）。
* 顶部右侧徽标实时显示上下文用量（`tokens/window` 与百分比）。
* 标题栏左侧状态文字（`online` / `offline`）与右侧圆点反映 WebSocket 连接状态；
  断线时发送按钮禁用，并每 1.5s 自动重连（重连前先向 `/api/session` 确认会话还有效；
  会话已失效则改为弹出登录对话框）。未登录（且配置了密码）时不会建立连接。
* **两种形态自适应**：手机（窄屏）为单列——紧凑顶栏 + 消息流 + 底部输入区；
  桌面（宽屏 ≥1000px）在左侧展开固定信息栏，含上下文进度条（随占用变黄/变红）、
  快捷操作（切换工具结果输出，与 `/result` 共享）与快捷键说明，右侧为聊天主区。
* 布局用 `100dvh` 动态视口高度 + 安全区（含顶部刘海与横屏左右内边距）适配手机；
  不使用 `position:fixed`，输入区始终可见；内容列居中且最宽 920px；点触目标不小于 44px。
* 回合运行中顶部显示转圈指示，输入框右侧出现 **Stop** 按钮：点击等同于发送 `/stop`，
  中断当前模型调用或工具调用；中断事件（`interrupted`）与工具反馈 `interrupted by user`
  会和其它事件一样广播给所有客户端。
* 转圈指示旁是**本回合用时**，格式与 CLI 提示符一致（`busyElapsedLocked`）：不足一分钟
  显示到 0.1 秒（`12.3s`），之后 `1m05s`、`1h02m`。计时随转圈一起开始、每 100ms 刷新
  （与 CLI 的动画节奏相同），`turn_done`/中断/断线即清零；运行中的 steering 消息不重启计时，
  但计时从**本页**看到回合开始算起，因此运行中刷新或重连会从零重新计时。
  转圈动画**不受**“减弱动态效果”偏好影响（该偏好只关闭消息行的入场动画）——它是运行中的
  唯一提示，停止转圈看起来像卡死。
* 工具调用行（`tool_call`）与 CLI 一致：**函数名单独一行高亮**（黄色加粗），参数**不缩进**地
  逐条列出——单行值紧跟参数名（`参数名 - 值`），多行值则参数名单独一行、值从下一行开始；
  参数名为青色加粗、分隔符与值为灰色（与 CLI 调色板对齐）。值用原始内容（字符串去掉引号，
  嵌套数组/对象为紧凑 JSON），解析失败时回退为原始文本。
* 工具（exec）结果按纯文本展示，保留换行；用 `/result off` 可隐藏。
* **滚动跟随按需停止**：日志区只在视图已位于底部时才自动跟随新输出；一旦向上拖动/滚动
  （回看或复制历史），后续输出仍会正常追加到下方，但**不再强制下拉**，滚回底部即恢复跟随
  （容差 8px，底部附近的微小偏移仍视作跟随）。发送消息被视为主动“看下文”，会重新贴底；
  重连收到的 `history` 快照重建整个日志区时同样贴底（等同刷新页面）。

## 连接维护

* 页面在连接断开后每 1.5s 自动重连。
* 服务端对每个连接独立读循环；写操作串行化并带写超时，写失败的连接会被关闭移除。
* 服务端实现 ping/pong（收到 ping 立即回 pong），并在收到 close 帧时正常收尾。

## 实现说明

`internal/web/ws.go` 用标准库实现了最小可用的 RFC6455：

* 握手：计算 `Sec-WebSocket-Accept = base64(sha1(key + GUID))`，通过 `http.Hijacker`
  接管连接并返回 `101 Switching Protocols`。
* 帧：支持 7/16/64 位长度、客户端掩码解码、分片重组；处理 ping/pong/close 控制帧。
* 单帧上限 8MB。
