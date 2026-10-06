# 配置

配置文件为 `config.json`，默认位于**可执行文件所在目录**（程序目录）。
环境变量 `LIGHTAGENT_CONFIG` 可覆盖其路径（开发时配合 `go run .` 很有用）。

命令行优先于环境变量与文件：

* `-c/--config PATH`：指定配置文件（优先于 `LIGHTAGENT_CONFIG`）。
* `-C/--dir PATH`：先切换工作目录，因此相对路径（`-c`、`--session`、`--log`）按新目录解析，
  `.lightagent/` 也建在新目录。
* 临时覆盖：`--model`、`--api-base`、`--stream on|off`、`--markdown on|off`、`--result on|off`、
  `--timeout SECONDS`、`--web-host`、`--web-port`、`--no-web`。
* `--print-config`：打印合并后的生效配置（`api_key` 打码）后退出，便于排查。
* **网页编辑**：Web 镜像的 ⚙ 按钮打开配置编辑器（`GET`/`PUT /api/config`），提供 **Form 控件表单**与
  **JSON 原文**两种模式（同一份文档、实时同步）；返回的文档按启动规则补全默认值、`api_key` 与
  `web.password` 打码（原样回传即保留原值），写回前做与启动相同的校验，因此文件始终可启动。
  `web.password` 行还有 **Set / Remove** 密码控件（`POST /api/password`，立即生效，盐随之轮换）；
  其余改动**写回后不立即生效，需重启 lightagent**；详见 [web.md](web.md#配置编辑apiconfig) 与
  [登录与鉴权](web.md#登录与鉴权)。

优先级：**命令行 flag > `LIGHTAGENT_CONFIG` > `config.json` > 内置默认**。

首次运行若文件不存在，会写入一份默认配置并提示编辑（至少填写 `providers[].api_key`）。

## 完整示例

```jsonc
{
  // LLM 接口列表：每个接口含身份（name / type / enabled）、请求形状与该模型的
  // 上下文窗口。启动时使用**第一个 enabled** 的接口；运行时可 /switchapi 切换
  // （只改内存，不写回文件）。type 目前只有 "openai"。
  "providers": [
    {
      "name": "default",
      "type": "openai",
      "enabled": true,
      "api_base": "https://api.openai.com/v1",
      "api_key": "sk-...",
      "model": "gpt-4o-mini",
      "temperature": -1.0,                 // 负值=不发送该字段（交由服务端默认）；0=确定性；>0=采样温度
      "max_tokens": 40960,
      "timeout_seconds": 4800,             // 空闲超时：无数据超过该秒数才中断（0=关闭）
      "stream": true,
      "media_types": ["image/png", "image/jpeg"],  // 模型可接收的多媒体类型（留空=不启用附件能力）
      "context_window": 131072,            // 该模型的上下文窗口（token）——它属于模型，故随接口走
      "extra_body": {
        "reasoning_effort": "high",
        "top_p": 0.95
      }
    }
  ],
  "context": {
    "summarize_token_percent": 75,
    // 压缩时保留多少最新消息：auto = 主动压缩，manual = /compact。两者都默认 0，
    // 即不保留任何原始消息：部分推理引擎在回退（请求前缀回到更早的位置）下不保存
    // prompt 缓存，把整段被压缩历史换成累积摘要，下一次请求才继续命中缓存。
    "summarize_keep": {
      "auto":   { "budget_percent": 0, "turns": 0 },
      "manual": { "budget_percent": 0, "turns": 0 }
    }
  },
  "web": {
    "host": "127.0.0.1",
    "port": 0,
    "password": ""                         // 登录密码（明文保存）；非空时程序再补上 password_salt
  },
  "tools": {
    "exec":            { "enabled": true, "timeout_seconds": 3600, "wait_seconds": 10, "max_lines": 50, "max_lines_max": 100, "use_utf8": true },
    "read_file_lines": { "enabled": true, "max_read_file_size": 32000, "max_read_file_lines": 200 },
    "write_file":      { "enabled": true, "max_lines": 200, "auto_split": true },
    "edit_file":       { "enabled": true },
    "webfetch":        { "enabled": true, "mode": "auto", "timeout_seconds": 30, "max_lines": 100 },
    "upload_media":    { "enabled": false, "max_bytes": 0 },
    "discovery":       { "enabled": false, "mode": "unlock", "ttl": 50, "max_search_results": 10, "min_match_rate": 0.5, "use_bm25": true },
    "mcp": {
      "enabled": false,
      "servers": {
        "filesystem": {
          "enabled": true,
          "command": "npx",
          "args": ["-y", "@modelcontextprotocol/server-filesystem", "."]
        },
        "remote": {
          "enabled": false,
          "type": "http",
          "url": "https://example.com/mcp",
          "headers": { "Authorization": "Bearer <token>" }
        }
      }
    }
  },
  "agent": {
    "max_tool_iterations": 200,
    "system_prompt": "",
    "include_working_dir": true,
    "summary_in_system_prompt": false,    // 发送时摘要的位置：默认第一条用户消息；true 则写进系统提示词
    "include_only_think": true,           // 保留「只有思考」的 assistant 消息（无正文、无工具调用）
    "continue_only_think": true           // 只到思考就停时再问一次模型；仅 include_only_think 为 true 时生效
  },
  "ui": {
    "markdown": true
  }
}
```

## 字段说明

### `providers`

`providers` 是**接口数组**（按顺序保存，序号从 1 起）。每个元素含三个身份字段，其余为该接口的请求形状：

| 身份字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `name` | string | 空 | 接口名；`/switchapi` 与 Web 侧栏的模型下拉都按它标识。空则自动补 `api-<序号>` |
| `type` | string | 空 | 接口类型，目前只有 `"openai"`；空视作 `openai`。其它值启动报错 |
| `enabled` | bool | `false` | 是否可用。**启动时取第一个 `enabled` 的接口**；没有启用的接口则启动报错 |

单接口的请求形状字段（与旧版 `openai` 块同名）：

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `api_base` | string | `https://api.openai.com/v1` | 接口基址；客户端会拼接 `/chat/completions`（若已以该路径结尾则直接使用） |
| `api_key` | string | 空 | 作为 `Authorization: Bearer <key>` 发送；启用中的接口为空则启动报错 |
| `model` | string | `gpt-4o-mini` | 模型名 |
| `temperature` | number | `-1` | 采样温度；`-1`（或任意负值）表示**不使用**——该字段不会发送，交由服务端默认；`0` 为确定性采样，`0` 以上的值会原样发送。省略该字段时即为 `-1` |
| `max_tokens` | int | `40960` | 单次回复上限 |
| `timeout_seconds` | int | `4800` | **空闲超时**（秒）：等待响应头、或流式过程中两个数据块之间的最大间隔；超过即中断并提示。不是整段请求的总时限，因此长回复不会被截断；`0` 关闭 |
| `stream` | bool | `true` | 是否使用 SSE 流式输出 |
| `media_types` | string[] | 空 | **本模型可接收的多媒体类型**（附件能力的总开关），如 `["image/png", "image/jpeg", "audio/wav", "application/pdf"]`。小写、可写族名（`"image"` = `"image/*"`）。留空 = 关闭附件能力：既不注册 `upload_media`，Web 输入框也没有附加按钮。非空时：Web 可附加文件，且若同时打开 `tools.upload_media.enabled` 则模型拿到上传工具（两边都为真才启用）。为空时不写入文件 |
| `context_window` | int | `131072` | **该模型的上下文窗口**（token），用于压缩触发与保留预算。它属于模型，因此随接口走：切换接口后压缩触发点随之变化。`0`（省略）取内置窗口 |
| `extra_body` | object | 无 | 见下节 |

> **切换当前接口（只存内存）**：启动时用第一个 `enabled` 接口；运行时可用 CLI 的
> `/switchapi <name|序号>`（无参列出接口）或 Web 侧栏的模型下拉切换。切换只更新内存中的
> 当前接口——**不写回 `config.json`**，下次启动仍取第一个启用的接口。切换时程序会换掉
> LLM 客户端、该接口的 `context_window` / `max_tokens`，并按新接口的 `media_types` 刷新
> 多媒体附件能力（Web 的附加按钮与 `upload_media` 工具）；忙碌（有回合在跑）时拒绝切换。

> **老版本自动迁移**：早期配置只有一个 `openai` 对象、且 `context.context_window` 在顶层。
> 读取到这种文件时，程序把它折成 `providers` 里的一个 `default` 接口（`type=openai`、
> `enabled=true`），并把顶层 `context_window` 搬进该接口，然后**把文件改写为新结构**，旧字段
> 不再写回。（`openai` 这个键仍可被解析/接受，但只用于这一次迁移。）

### `context`

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `summarize_token_percent` | int | `75` | 用量达到**当前接口 `context_window`** 的该百分比触发压缩（1–100） |
| `summarize_keep.auto.budget_percent` | int | `0` | 自动压缩保留的 token 预算占比（0–100）：可用输入预算 `context_window - max_tokens` 的百分比；`0` = 不保留原始消息 |
| `summarize_keep.auto.turns` | int | `0` | 自动压缩最多保留几个完整 Turn；`0` = 不保留 |
| `summarize_keep.manual.budget_percent` | int | `0` | 手动 `/compact` 的预算占比（同上） |
| `summarize_keep.manual.turns` | int | `0` | 手动 `/compact` 最多保留几个完整 Turn |

> **用量是什么**：接口返回的 `usage.prompt_tokens`（上一次请求的真实 token 数）**加上**此后追加
> 消息的**下限估算**（英语单词/代码片段按空格切分、中文逐字，各 1 个 token，每多 4 个字符再加 1 个）。
> 没有上报值时（刚启动、刚恢复会话）才退回对整份请求的估算。
> 整份历史的字符估算**不再**与上报值取大——两把尺相比会让压缩在设置百分比之外提前触发。

> **压缩保留多少最新消息可配置，默认一条都不留**：`context.summarize_keep.{auto,manual}` 的
> `budget_percent`（占可用输入预算 `context_window - max_tokens` 的百分比，二者取自当前接口）与 `turns`
> （最多保留几个完整 Turn，谁先触顶谁停）默认都是 `0`。原因是**部分推理引擎在回退下不保存 prompt
> 缓存**——不留原始消息时，压缩后的下一次请求就是「系统提示词 + 摘要」开头的实时前缀，缓存因此仍然
> 有效，而被放弃的最新几轮对话本身已经写进摘要。调大只对**能跨回退保住缓存**的服务商有意义。
> 切分仍按 Turn 边界进行，因此 `assistant.tool_calls` 与 `tool` 结果不会被切开；详见
> [architecture.md](architecture.md#保留summarizetailcut)。
> 溢出恢复（服务商以「上下文超限」拒绝请求后的一次压缩）**没有配置项**、永远不保留原始消息；详见
> [architecture.md](architecture.md#溢出恢复provider-拒绝后回退--摘要--重发)。

### `web`

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `host` | string | `127.0.0.1` | 监听地址；空/省略即仅本机。设为 `0.0.0.0` 或某个本机地址可对局域网开放 |
| `port` | int | `0` | `>0` 时启动 Web 镜像；被占用则自动 +1 递增（最多 +50） |
| `password` | string | 空 | 登录密码，**明文保存**（服务端据此推导浏览器发来的加盐摘要）；非空即要求登录。该字段**总是写入文件**，清除登录后是空字符串 |
| `password_salt` | string | 空 | 公开盐（32 位十六进制）；写 `password` 后由程序自动生成，改密码时轮换。**为空时不写入文件**，因此清除密码后它随之从文件里消失 |

### `tools`

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `exec.enabled` | bool | `true` | 启用 `run_script` 与 `manage_session` |
| `exec.timeout_seconds` | int | `3600` | `run_timeout` 的默认硬超时 |
| `exec.wait_seconds` | int | `10` | `wait_timeout` 的默认同步等待秒数 |
| `exec.max_lines` | int | `50` | `run_script` / `manage_session` 的 `max_lines` **参数的默认值**。一次回答最多带多少行。`0`/负数回退到内置 50 |
| `exec.max_lines_max` | int | `100` | 上面那个参数的**最大值**。调用要得更多就截断到它。`0`/负数回退到内置 100。小于 `max_lines` 时把 `max_lines` 降到它 |
| `exec.use_utf8` | bool | `true` | **仅 Windows**：stdio 编码模式（默认值，可被 `run_script` 的同名参数按次覆盖）：`true` 强制脚本引擎（PowerShell 与 Python）使用 UTF-8，Go 不转码；`false` 由 agent 按主机 ANSI 代码页（`GetACP`）自动解码，其余行为相同，但非本地 ANSI 字符可能无法显示 |
| `read_file_lines.enabled` | bool | `true` | 启用行读取工具 |
| `read_file_lines.max_read_file_size` | int | `32000` | 单次读取字节预算 |
| `read_file_lines.max_read_file_lines` | int | `200` | 单次读取行数预算 |
| `write_file.enabled` | bool | `true` | 启用写文件工具 |
| `write_file.max_lines` | int | `200` | 单次写入行数上限（超出截断） |
| `write_file.auto_split` | bool | `true` | 模型一次写出的文本超过 `max_lines` 时，由 agent 自动拆成多次写入（见 [tools.md](tools.md#write_file)）；关闭后恢复"截断 + 提示续写"的旧行为 |
| `edit_file.enabled` | bool | `true` | 启用编辑工具 |
| `webfetch.enabled` | bool | `true` | 启用 `webfetch`（抓网页 → Markdown） |
| `webfetch.timeout_seconds` | int | `30` | 单次抓取的秒数上限（也是工具 `timeout` 参数的默认值，会写进工具 schema）。低于 30 秒的值会被抬到 30 秒：实际超时不会小于内置下限（见 [tools.md](tools.md#webfetch)） |
| `webfetch.mode` | string | `"auto"` | 取页面的方式：`auto`（默认）= 用**可见窗口**的浏览器渲染、没有可用浏览器时回退 HTTP 源码；`chrome-headful` 必须用可见窗口渲染；`chrome-headless` 必须用无头渲染；`chrome-attached` 挂到已在运行的浏览器；`http` 只取源码、从不启动浏览器。`auto`/`chrome-headful`/`chrome-headless` 都复用工作目录下 agent 自己的 profile（`.lightagent/browser-profile`，跨抓取保留 Cookie，见 [tools.md](tools.md#webfetch)）。非以上取值校验失败（旧值 `browser` 也已被拒绝） |
| `webfetch.max_lines` | int | `100` | 反馈给模型的 Markdown 行数上限（**只影响默认的 `method=fetch_as_md`**；调用的 `method` 还可以选落盘或只要状态行，见 [tools.md](tools.md#四种反馈方式method)）。超长的页面**优先只回填正文**（`<article>` / `<main>` / `class=main` 之类的容器，导航、侧栏、页脚留在外面），反馈里用标记写出被省略了多少行与正文起始行号；定位不到正文时回填**中间 N 行**。两种情况下整页（**Markdown 正文**）都写到工作目录的 `.lightagent/webfetch/<时间戳>.md`，状态行报告总行数/总字节、这次取的是哪一段与文件路径（见 [tools.md](tools.md#反馈长度与正文定位toolswebfetchmax_lines)）。`0` 用内置 100，负数 = 不限长度且不落盘 |
| `webfetch.browser_path` | string | 空 | 渲染用的浏览器可执行文件；为空时自动探测已安装的浏览器。为空时不写入文件 |
| `webfetch.user_agent` | string | 空 | 覆盖两条路径的 User-Agent（浏览器渲染时由浏览器发送、HTTP 源码是请求头）；为空时各用自带默认（Go 客户端 / 浏览器自身）。为空时不写入文件 |
| `webfetch.max_bytes` | int | `0` | HTTP 源码正文的字节上限；`0` 用内置的 8 MiB。负数回退到 `0`；为 `0` 时不写入文件 |
| `webfetch.attach_address` | string | 空 | `chrome-attached` 挂载的 DevTools 端点，一个字符串即可：端口 `9222`（= `127.0.0.1:9222`）、`192.168.0.5:9223`、`http://…` 或 `ws://…`。为空时用内置的 `127.0.0.1:9222`；为空时不写入文件 |
| `upload_media.enabled` | bool | `false` | 启用 `upload_media`（把本地多媒体文件上传成对话附件）。**必须与当前接口的 `providers[].media_types` 同时成立**：只配上类型而不打开该开关，或只打开开关而不配类型，都不会注册这个工具 |
| `upload_media.max_bytes` | int | `0` | 单个附件文件的字节上限；`0` 用内置的 20 MiB。负数回退到 `0`；为 `0` 时不写入文件。**Web 附加按钮用同一个上限**（超限的文件在浏览器里就被拒，不发往服务端） |

* `exec.use_utf8` 默认为 `true`：加载时先取默认值再合并文件，**省略该字段即保持开启**；
  需要旧的 ANSI 代码页转换时显式写 `"use_utf8": false`。它只是默认值——`run_script` 的
  `use_utf8` 参数可由模型按次调用覆盖。该字段与参数**只在 Windows 生效**（非 Windows 无 ANSI
  代码页可回退，始终 UTF-8）。`run_script` 的脚本语言由 `language` 参数选择（宿主引擎
  `ps`/`sh`，以及系统存在 Python 时的 `python`），细节见 [tools.md](tools.md#run_script)。

* `exec.max_lines` / `exec.max_lines_max` 是同一对参数（默认值 / 最大值）。两者一起管
  `run_script` 与 `manage_session` 的 `max_lines`。省略参数时用 `max_lines`。要得比
  `max_lines_max` 多就截断到 `max_lines_max`。两者都会写进工具 schema，模型看得见自己的预算。
  `max_lines_max` 小于 `max_lines` 时，加载时把 `max_lines` 降到它。重要输出建议重定向到文件，
  再用文件工具读回。

* `webfetch.mode` / `browser_path` / `user_agent` / `max_bytes` / `attach_address` 只管**取页面**这一步：
  转换（HTML → Markdown）、反馈长度限制与结果格式都不受影响。工具描述按 `mode` 如实说明取法
  （`http` 时不会声称会渲染浏览器，`chrome-headless` 时不会说会开窗口），
  实际走的那条路由抓取结果里的 `Method` / `Notes` 报告，CLI 与网页的展示行也会带上。
* `webfetch` **只抓网页与文本**：地址返回二进制内容（PDF、图片、压缩包等）时直接返回错误结果并提示
  改用 `run_script`，HTTP 路径在读取正文前就按 `Content-Type` 拒绝。
* 启动浏览器的模式（`auto` / `chrome-headful` / `chrome-headless`）用**工作目录下**
  `.lightagent/browser-profile` 作为 profile（agent 自己的 Cookie 与登录态，随项目走），
  不碰你自己的浏览器 profile；该目录已被 `.gitignore` 忽略。

#### `providers[].media_types` / `tools.upload_media`（多媒体附件）

多媒体能力由**两个开关**共同决定，缺一不可：

1. `providers[].media_types`：**当前接口模型能读的媒体类型**（能力总开关）。它是判断"这个文件能不能交给模型"的
   唯一依据，也决定 Web 附加按钮是否存在、文件选择器只筛哪些类型。切换接口时会按新接口的类型刷新。
2. `tools.upload_media.enabled`：**是否把这个能力做成工具**给模型用（模型可以自己指定路径上传）。

四条通路：

| `media_types` | `upload_media.enabled` | 模型侧 `upload_media` 工具 | Web 附加文件 |
|---------------|------------------------|---------------------------|--------------|
| 空 | 任意 | 不注册 | 关闭（输入框没有附加按钮，`/api/upload` 直接 404） |
| 非空 | `true` | **注册**（描述与 `path` 参数里都列出可接收类型） | 可用 |
| 非空 | `false` | 不注册（模型无法自行上传） | 可用（用户附加的附件仍然照发） |

* 类型的匹配：小写比较、忽略 `; charset=…` 之类的参数；族名（没有 `/` 的写法）等价于通配，
  即 `"image"` = `"image/*"`；`"image/*"` 匹配任意 `image/…`。
* 类型判定：**先看扩展名、再看文件头**（`mime.TypeByExtension` + `http.DetectContentType`），
  两个候选里只要有一个被接受就用它，因此**没有扩展名的文件也能被认出来**；两个都不是可接收类型时报错，
  错误信息里会列出可接收类型。扩展名与内容矛盾时（比如 `.txt` 里其实是 PNG）以扩展名优先。
* 上限：`tools.upload_media.max_bytes`（默认 20 MiB）对**工具与 Web 两条路都一样**：
  超限的文件不读、不存、不发。
* 附件的载荷形态见 [tools.md](tools.md#upload_media)：图片走 `image_url`（data URI）、
  音频走 `input_audio`（wav/mp3）、其余（PDF、视频等）走 `file`（文件名 + data URI）。

#### `tools.discovery`（MCP 工具发现 / unlock）

MCP unlock 发现机制：**锁定函数**（deferred）默认不下发给模型、不可直接调用；模型先用 BM25 搜索发现函数名，再用 `unlock_tool` 激活拿到完整 schema，最后经 `dynamic_call` 间接调用。**MCP 工具恒定走这套机制**（见 `tools.mcp`），lightagent 自带工具则始终作为核心工具暴露。

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `enabled` | bool | `false` | 仅当宿主**自行**通过 API 注册 deferred 工具时才需要显式开启；MCP 会自动启用该机制。控制面与提示词是否出现只取决于**是否确实存在锁定函数** |
| `mode` | string | `"unlock"` | 仅支持 `unlock`（lightagent 未实现 classic 提升模式） |
| `ttl` | int | `50` | `unlock_tool` 授权的保持轮数；**每个工具执行轮结束**递减一次，归零后函数重新锁定但仍可被搜索发现 |
| `max_search_results` | int | `10` | 单次 BM25 搜索返回的最大函数数 |
| `min_match_rate` | float | `0.5` | 关键词匹配率下限：函数必须命中的查询关键词占比（`0`~`1`），低于它的一律不返回。查询里**重复的关键词按出现次数各算 1 个词**，重复词既让分母变大也让权重变高。`0`（未配置）或大于 `1` 的取值回退到默认值 |
| `use_bm25` | bool | `true` | 启用 BM25 自然语言搜索；lightagent 只有这一个发现搜索工具，故必须为 `true` |

> * 控制面（`tool_search_tool_bm25` / `unlock_tool` / `dynamic_call`）与系统提示词里的机制说明、MCP 全局信息**只在存在锁定函数时**出现；没有锁定函数时不会往上下文插入任何多余内容。
> * `mode` 非 `unlock` 或 `use_bm25=false`（在 `enabled=true` 或 `mcp.enabled=true` 时）会导致配置校验失败。

#### `tools.mcp`（MCP 客户端）

lightagent 内置一个**纯标准库**的 MCP 客户端。启动时按 `servers` 连接每个 `enabled` 的 server，完成 `initialize` 握手并拉取 `tools/list`。每个 server 工具被包装成名字为 `mcp_<server>_<tool>` 的工具，并**始终作为锁定函数（deferred）注册**：它不会出现在模型的 `tools` 声明里，搜索只回报名称，只有 `unlock_tool` 的返回值才会把完整 schema 带进对话。

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `mcp.enabled` | bool | `false` | 总开关；关闭时不建立任何连接 |
| `mcp.servers` | object | `{}` | server 名 → server 配置；名字会用于工具前缀（`mcp_<server>_…`） |

为空时会自动插入一个名为 `example` 的 **disabled 实例**（模板，见下），方便直接照着改；只要已声明任一 server 就不会插入。

每个 server 配置：

| 字段 | 类型 | 说明 |
|------|------|------|
| `enabled` | bool | 必须显式设为 `true` 才会连接 |
| `type` | string | `stdio` / `http`（别名 `streamable-http`）/ `sse`；省略时：有 `command` → `stdio`，仅有 `url` → `http` |
| `command` | string | stdio：可执行文件（如 `npx`、`python`、绝对路径） |
| `args` | string[] | stdio：命令参数 |
| `env` | object | stdio：追加/覆盖的环境变量 |
| `env_file` | string | stdio：.env 风格文件（`KEY=value`），相对路径按进程工作目录解析 |
| `url` | string | http / sse：服务地址 |
| `headers` | object | http / sse：附加请求头（如鉴权） |

> * 传输支持：**stdio**（子进程、按行分隔 JSON-RPC）、**Streamable HTTP**（每次请求一个 POST，响应为 JSON 或 SSE，回传并复用 `Mcp-Session-Id`）、**legacy HTTP+SSE**（长连接事件流 + endpoint POST）。
> * 单个 server 连接失败会在启动时打印 `mcp: …` 并继续，不影响其他 server。
> * 校验：`mcp.enabled=true` 时，stdio server 需要 `command`，http/sse server 需要 `url`，否则配置校验失败。
> * 启动只往系统提示词注入两类“机制说明”：一条**全局 unlock 规则**（只注入 1 次）+ 每个连上的 server 一条 **MCP 全局信息**。MCP 全局信息 = 配置键名 + 工具数 + `registered as locked tools; … unlock_tool … dynamic_call`，再附上 **server 在 initialize 里返回的信息**（`serverInfo.name/title/version` 与可选的 `instructions`）。二者都不列出任何函数名。
> * MCP 工具**永远**是锁定函数：搜索发现不注册、不授权；只有 `unlock_tool` 才下发 schema 并授权；授权过期后重新 `unlock_tool` 即可。

### `agent`

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `max_tool_iterations` | int | `200` | 单个回合内工具循环的最大轮数 |
| `system_prompt` | string | 空 | 自定义系统提示词；空则用内置默认（`agent.md` 优先级更高）。运行时环境行由程序自动追加，不必写在这里 |
| `include_working_dir` | bool | `true` | 启动时把**工作目录行**（仅进程所在目录的绝对路径，不列目录内容）追加到系统提示词末尾。省略即开启，显式 `false` 关闭 |
| `summary_in_system_prompt` | bool | `false` | 发送时累积摘要的位置：`false` = 第一条用户消息（`[engine] CONVERSATION SUMMARY:` 开头）；`true` = 系统提示词末尾的 `# CONVERSATION SUMMARY` 段 |
| `include_only_think` | bool | `true` | 一条**只有思考**（可见正文为空、无工具调用）的 assistant 回复是否记入历史。省略即开启，显式 `false` 关闭；关闭时这条消息被丢弃 |
| `continue_only_think` | bool | `true` | 模型「只到思考就停」时（`finish_reason=stop`），是否记一条 `info` 并**带着这段思考**再问一次模型，而不是结束回合。默认开启，且**仅在 `include_only_think=true` 时生效**（没记下的思考无法带入重试） |

`include_working_dir` 插入的段落只有一行，即进程的工作目录：

```
working directory: D:\work\demo
```

目录里的子目录与文件**不再列出**（无论项目多大，这一段都是一行），需要时由模型用文件工具查看。

由于加载时先取默认值再合并文件，省略 `include_working_dir` 即保持开启；关闭需显式写 `"include_working_dir": false`。

`summary_in_system_prompt` 决定发送时累积摘要的位置，默认 `false`（第一条用户消息）：

```
system: <基础提示词 + unlock 规则 + MCP 信息 + 运行时行 + 工作目录行>
user:   [engine] CONVERSATION SUMMARY:
        <累积摘要>
user:   <历史里的第一条 user>
...
```

设为 `true` 则把摘要追加到系统提示词末尾的 `# CONVERSATION SUMMARY` 段。该开关只作用于发送前
组装的消息列表（`history`、会话文件与 CLI/web 显示不变）。细节见
[architecture.md](architecture.md#摘要的放置agentsummary_in_system_prompt)。

### 「只有思考」的回复（`include_only_think` / `continue_only_think`）

有的服务商会让模型**先吐思考、随后直接结束**（可见正文为空、没有工具调用）。这类回复**无论
`finish_reason` 是什么**都走同一通路 —— 正常结束、`max_tokens` 截断（`length`）、或没有给出，处理
完全一致。两个开关决定怎么处理，默认都开启：

* 默认：这条回复作为 assistant 消息（`reasoning_content`，无正文）**记入历史**，发布一条 `info`
  （`the reply carried only thinking; asking the model again`），然后**带着这段思考再问一次模型** ——
  若模型只是提前停住，这次通常就能给出正文。思考因此会随后续请求一起回传（配合 preserve thinking 模板）。
* `include_only_think=false`：这条消息**不记**入历史，回合就此结束；`continue_only_think` 此时不生效。
* `include_only_think=true`、`continue_only_think=false`：消息**记入**历史，但不再多问一次。
* 只有**带了正文**的 `finish_reason=length` 截断才走「截断续跑」（`response truncated at max_tokens;
  continuing (n/3)`）。
* 上述两种自动续跑（**只有思考**、**`length` 截断**）**共用同一个连续计数**：连续 **3** 次即报
  `error`（`stopped after 3 consecutive continuations ...`）并结束回合；中途出现工具轮次会把计数清零。
* **中断**（用户 / stop）时同样受 `include_only_think` 影响：开启且已流出思考时，思考被保留为一条
  assistant 消息（其中携带的 tool 调用照旧全部丢弃）；关闭、或思考也为空时整条消息不保留。细节见
  [architecture.md](architecture.md#agent-回合循环)。

### `ui`

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `markdown` | bool | `true` | 把助手回答与模型思考（`[thinking]`）渲染为 Markdown：CLI 转 ANSI 着色，Web 由浏览器端 marked（GFM，含表格）+ DOMPurify 渲染。设为 `false` 则原样输出纯文本 |

* 由于加载时先取默认值再合并文件，**省略 `ui` 段即保持开启**；关闭需显式写 `"markdown": false`。
* 运行中可用 `/markdown off` / `/markdown on` 临时切换（仅影响当前进程，不写回配置）。
* Windows 旧版控制台（cmd.exe / Windows PowerShell 宿主）不支持 ANSI 时颜色会自动关闭，
  此时 Markdown 仍然生效，只是以纯文本形式呈现（标记被去掉，不带颜色）。

## `extra_body`：OpenAI 额外请求参数

不同服务商常需要额外的请求体字段（如 `reasoning_effort`、`top_p`、`response_format`、
`chat_template_kwargs` 等）。把它们写进某个接口的 `extra_body`（`providers[].extra_body`），lightagent 会在发送请求时
把这些键**合并到 `/chat/completions` 请求体的顶层**。

```jsonc
"extra_body": {
  "reasoning_effort": "high",     // 顶层新增字段
  "temperature": 0.2              // 覆盖内置 temperature
}
```

* 合并发生在序列化之后，因此 `extra_body` 中的值会覆盖同名的内置字段。
* 该参数同时作用于正常对话与压缩摘要调用。

## 系统提示词覆盖：`agent.md`

程序目录下的 `agent.md` 若存在且非空，会**自动覆盖**内置提示词：

优先级：`agent.md` > `config.agent.system_prompt` > 内置默认。

程序会在基础提示词之后**自动按顺序追加能力段落** —— unlock 规则、MCP 信息、运行时环境行
`Runtime: <GOOS>/<GOARCH>.`、工作目录行 `working directory: <绝对路径>`（后者由
`agent.include_working_dir` 控制，默认开启）—— 因此 `agent.md` / `agent.system_prompt`
**不需要也不应**写这些内容：
`gen-agent-prompt` 导出的模板已不再包含它们（旧模板里残留的 `Runtime: ...` 行可以直接删掉）。

### 片段导入：`@include("路径")`

`agent.md` 中**独占一行**的 `@include("路径")`（允许行首缩进与尾随空白）会被替换为该路径的内容：

```markdown
你是 Light Agent。

@include("prompts/style.md")          # 单个文件
@include("prompts/rules")             # 目录：插入其下所有文件
@include("C:/shared/company.md")      # 绝对路径
```

* **路径解析**：相对路径基于**指令所在文件**的目录（不是工作目录，也不是 agent.md 的目录）；
  绝对路径直接使用。
* **目录**：插入该目录下**所有文件**（含子目录，按路径字典序），文件之间插入一个空行（`\n\n`）；
  空目录不产生任何内容，该指令行被整行移除。
* **递归**：被插入的文件内同样可以写 `@include`，可多层嵌套。
* **限制**：嵌套上限 16 层；自引用/循环引用会报错并打印循环链；目标缺失也会报错。这类错误会
  中止配置加载（报错形如 `lightagent: load <agent.md 路径>: include ...`），而不是静默回退内置提示词。
* 只有**整行**指令会被展开，正文中提及 `@include("...")` 的普通文本保持原样。

导出内置模板：

```bash
lightagent gen-agent-prompt        # 生成 agent.md；已存在则拒绝
lightagent gen-agent-prompt -f     # 强制覆盖
```

生成的 `agent.md` 即为可直接编辑的提示词文本（不会自动加入注释，避免污染提示词）。

## 配置优先级与默认值

1. 读取 `config.json`；缺失字段使用内置默认（`config.Default()`）。
2. **启动时自动对齐**：若文件缺失任何内置字段（或 `tools.mcp.servers` 为空），补全后写回
   `config.json`；文件本来完整时**不写回**，因此内容与修改时间都不变。
3. 非法/越界值回退默认：`summarize_token_percent` 不在
   `(0,100]` 时回退；`summarize_keep` 的 `budget_percent` 不在 `[0,100]`、`turns` 为负时回退
   （默认两者都是 0，即不保留原始消息）。
4. 若存在 `agent.md`，覆盖 `agent.system_prompt`（文件中的 `@include` 会先展开）。
5. `ui.markdown`、`agent.include_working_dir`、`agent.include_only_think`、`agent.continue_only_think`
   默认 `true`，仅在文件中显式写 `false` 才会关闭；
   `agent.summary_in_system_prompt` 反之默认 `false`（摘要作为独立的 `[engine]` 消息紧跟系统提示词），显式写 `true` 才放进系统提示词。
