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

首次运行若文件不存在，会写入一份默认配置并提示编辑（至少填写 `openai.api_key`）。

## 完整示例

```jsonc
{
  "openai": {
    "api_base": "https://api.openai.com/v1",
    "api_key": "sk-...",
    "model": "gpt-4o-mini",
    "temperature": 0.0,
    "max_tokens": 40960,
    "timeout_seconds": 4800,               // 空闲超时：无数据超过该秒数才中断（0=关闭）
    "stream": true,
    "extra_body": {
      "reasoning_effort": "high",
      "top_p": 0.95
    }
  },
  "context": {
    "context_window": 131072,
    "summarize_token_percent": 75
  },
  "web": {
    "host": "127.0.0.1",
    "port": 0,
    "password": "",
    "password_salt": ""
  },
  "tools": {
    "exec":            { "enabled": true, "timeout_seconds": 3600, "wait_seconds": 10, "use_utf8": true },
    "read_file_lines": { "enabled": true, "max_read_file_size": 32000, "max_read_file_lines": 200 },
    "write_file":      { "enabled": true, "max_lines": 200 },
    "edit_file":       { "enabled": true },
    "discovery":       { "enabled": false, "mode": "unlock", "ttl": 50, "max_search_results": 50, "use_bm25": true },
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
    "include_working_dir": true
  },
  "ui": {
    "markdown": true
  }
}
```

## 字段说明

### `openai`

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `api_base` | string | `https://api.openai.com/v1` | 接口基址；客户端会拼接 `/chat/completions`（若已以该路径结尾则直接使用） |
| `api_key` | string | 空 | 作为 `Authorization: Bearer <key>` 发送；为空则启动报错 |
| `model` | string | `gpt-4o-mini` | 模型名 |
| `temperature` | number | `0.0` | 采样温度 |
| `max_tokens` | int | `40960` | 单次回复上限 |
| `timeout_seconds` | int | `4800` | **空闲超时**（秒）：等待响应头、或流式过程中两个数据块之间的最大间隔；超过即中断并提示。不是整段请求的总时限，因此长回复不会被截断；`0` 关闭 |
| `stream` | bool | `true` | 是否使用 SSE 流式输出 |
| `extra_body` | object | 无 | 见下节 |

### `context`

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `context_window` | int | `131072` | 模型上下文窗口（token），用于压缩触发与保留预算 |
| `summarize_token_percent` | int | `75` | 用量达到 `context_window` 的该百分比触发压缩（1–100） |

> **保留多少最新消息由算法推导，无需配置**：token 预算 =
> `(context_window - openai.max_tokens)` 除以 10（自动压缩）或 20（手动 `/compact`），
> 且最多保留 3 个（自动）或 2 个（手动）完整 Turn。详见
> [architecture.md](architecture.md#保留summarizetailcut)。

### `web`

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `host` | string | `127.0.0.1` | 监听地址；空/省略即仅本机。设为 `0.0.0.0` 或某个本机地址可对局域网开放 |
| `port` | int | `0` | `>0` 时启动 Web 镜像；被占用则自动 +1 递增（最多 +50） |
| `password` | string | 空 | 登录密码，**明文保存**（服务端据此推导浏览器发来的加盐摘要）；非空即要求登录 |
| `password_salt` | string | 空 | 公开盐（32 位十六进制）；写 `password` 后由程序自动生成，改密码时轮换 |

### `tools`

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `exec.enabled` | bool | `true` | 启用 `exec_command` 与 `manage_session` |
| `exec.timeout_seconds` | int | `3600` | `run_timeout` 的默认硬超时 |
| `exec.wait_seconds` | int | `10` | `wait_timeout` 的默认同步等待秒数 |
| `exec.use_utf8` | bool | `true` | **仅 Windows**：stdio 编码模式（默认值，可被 `exec_command` 的同名参数按次覆盖）：`true` 强制脚本引擎（PowerShell 与 Python）使用 UTF-8，Go 不转码；`false` 由 agent 按主机 ANSI 代码页（`GetACP`）自动解码，其余行为相同，但非本地 ANSI 字符可能无法显示 |
| `read_file_lines.enabled` | bool | `true` | 启用行读取工具 |
| `read_file_lines.max_read_file_size` | int | `32000` | 单次读取字节预算 |
| `read_file_lines.max_read_file_lines` | int | `200` | 单次读取行数预算 |
| `write_file.enabled` | bool | `true` | 启用写文件工具 |
| `write_file.max_lines` | int | `200` | 单次写入行数上限（超出截断） |
| `edit_file.enabled` | bool | `true` | 启用编辑工具 |

* `exec.use_utf8` 默认为 `true`：加载时先取默认值再合并文件，**省略该字段即保持开启**；
  需要旧的 ANSI 代码页转换时显式写 `"use_utf8": false`。它只是默认值——`exec_command` 的
  `use_utf8` 参数可由模型按次调用覆盖。该字段与参数**只在 Windows 生效**（非 Windows 无 ANSI
  代码页可回退，始终 UTF-8）。`exec_command` 的脚本语言由 `language` 参数选择（宿主引擎
  `ps`/`sh`，以及系统存在 Python 时的 `python`），细节见 [tools.md](tools.md#exec_command)。

#### `tools.discovery`（MCP 工具发现 / unlock）

MCP unlock 发现机制：**锁定函数**（deferred）默认不下发给模型、不可直接调用；模型先用 BM25 搜索发现函数名，再用 `unlock_tool` 激活拿到完整 schema，最后经 `dynamic_call` 间接调用。**MCP 工具恒定走这套机制**（见 `tools.mcp`），lightagent 自带工具则始终作为核心工具暴露。

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `enabled` | bool | `false` | 仅当宿主**自行**通过 API 注册 deferred 工具时才需要显式开启；MCP 会自动启用该机制。控制面与提示词是否出现只取决于**是否确实存在锁定函数** |
| `mode` | string | `"unlock"` | 仅支持 `unlock`（lightagent 未实现 classic 提升模式） |
| `ttl` | int | `50` | `unlock_tool` 授权的保持轮数；**每个工具执行轮结束**递减一次，归零后函数重新锁定但仍可被搜索发现 |
| `max_search_results` | int | `50` | 单次 BM25 搜索返回的最大函数数 |
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
| `include_working_dir` | bool | `true` | 启动时把**当前目录清单**插入系统提示词（工作目录 + 直接子项；子目录附其直接子项数量）。省略即开启，显式 `false` 关闭 |

`include_working_dir` 插入的段落形如（子目录在前并带 `[直接子项数量]`，随后是文件）：

```
working directory: D:\work\demo
internal[4]
docs[12]
main.go
go.mod
```

由于加载时先取默认值再合并文件，省略 `include_working_dir` 即保持开启；关闭需显式写 `"include_working_dir": false`。

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
`chat_template_kwargs` 等）。把它们写进 `openai.extra_body`，lightagent 会在发送请求时
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

程序会在基础提示词之后**自动追加运行时环境行** `Runtime: <GOOS>/<GOARCH>.`（以及可选的目录清单、
unlock 规则、MCP 信息），因此 `agent.md` / `agent.system_prompt` **不需要也不应**写这一行：
`gen-agent-prompt` 导出的模板已不再包含它（旧模板里残留的 `Runtime: ...` 行可以直接删掉）。

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
3. 非法/越界值回退默认：`temperature=0` 视为未设置；`summarize_token_percent` 不在
   `(0,100]` 时回退；负的 `keep_recent_messages` 回退。
4. 若存在 `agent.md`，覆盖 `agent.system_prompt`（文件中的 `@include` 会先展开）。
5. `ui.markdown` 与 `agent.include_working_dir` 默认 `true`，仅在文件中显式写 `false` 才会关闭。
