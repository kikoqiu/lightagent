# lightagent

一个使用glang的**微型、独立、超轻量级**的跨平台单文件命令行 AI Agent，并拥有一个漂亮的web cli镜像。

它通过 OpenAI 兼容接口驱动一个「对话 → 工具调用 → 观察结果 → 继续对话」的循环，
支持在循环过程中插入新的用户消息（steering），并在需要时对上下文做统一压缩。


## 详细文档

* [架构 architecture.md](docs/architecture.md)
* [配置 configuration.md](docs/configuration.md)
* [工具 tools.md](docs/tools.md)
* [Web 镜像 web.md](docs/web.md)
* [开发指南 development.md](docs/development.md)

---

## 特性

| # | 能力 | 说明 |
|---|------|------|
| 1 | OpenAI 兼容接口 | `chat/completions`，支持 `tools` / `tool_calls`，默认流式（SSE） |
| 2 | 全局配置 | 程序目录下的 `config.json`：接口、上下文长度、web、工具限制 |
| 3 | 运行状态在 CWD | 所有状态/记录写入**当前工作目录**的 `.lightagent/`，支持多实例 |
| 4 | CLI 工具循环 | 彩色 REPL，循环期间可继续输入插入用户消息 |
| 5 | Web 镜像 | 配置了 web 端口即启动（`web.host` 可选绑定 IP，默认 `127.0.0.1`）；端口被占用时自动递增；**WebSocket 实时双向镜像**；页面内置配置编辑器（控件表单 + JSON 双模式，`/api/config`，**重启生效**）；**登录对话框**（密码明文保存在配置里、浏览器只发送加盐摘要，HttpOnly 会话 cookie + 记住我，**改密码立即生效**） |
| 6 | 会话记录 | **一个目录一个会话**：启动询问是否恢复历史（默认是），退出询问是否保存（默认是），运行中只存内存，`/save` 手动落盘 |
| 7 | 命令执行 | `exec_command` / `manage_session`（脚本语言 `ps`/`sh`/`python`，后台会话、轮询、输入、终止）；`/result` 可开关结果输出 |
| 8 | 文件操作 | `read_file_lines` / `write_file` / `edit_file`（含 GBK 等字符集转换） |
| 9 | 上下文压缩 | 全局唯一压缩模式：超阈值时把旧消息总结成一条摘要；`/history` 查看用量，CLI 提示行与网页徽标实时显示 |
| 10 | 彩色 CLI | 角色区分颜色；仅在确认终端支持 ANSI 时才着色（非 TTY、`NO_COLOR`、`TERM=dumb`、旧版 Windows 控制台自动关闭） |
| 11 | Markdown 渲染 | 助手回答与模型思考均渲染为 Markdown：CLI 转 ANSI，Web 用浏览器端 marked（GFM，含表格）+ DOMPurify；`ui.markdown` 默认开启 |
| 12 | 多行输入 | CLI 与 Web 均为 **Enter 换行**；CLI 发送用 **Ctrl+J**（Windows 本地也可 Ctrl+Enter；Linux/SSH 下终端支持 kitty 键盘协议时也可 Ctrl+Enter，另有 Alt+Enter），Web 用 **Ctrl+Enter** |
| 13 | MCP 客户端 + 工具发现 / unlock | 纯标准库 MCP 客户端（stdio / Streamable HTTP / HTTP+SSE），server 在 `tools.mcp` 中配置；MCP 工具**恒为锁定函数**（永不进 `tools` 声明）：`tool_search_tool_bm25` 发现 → `unlock_tool` 下发 schema 并授权（TTL）→ `dynamic_call` 间接调用；系统提示词只注入 1 条全局 unlock 规则 + 每 server 1 条 MCP 全局信息（含 server 返回的 `serverInfo`/`instructions`） |
| 14 | 思考流式展示 | 服务商返回 `reasoning_content` / `reasoning` 时，CLI 以 `[thinking]` 块、Web 以 thinking 行**实时流式**展示模型思考（与回答一样按 `ui.markdown` 渲染 Markdown）；定稿后的思考随 assistant 消息写入历史、以 `reasoning_content` 回传（配合 preserve thinking 模板） |
| 15 | 系统提示词 | 超短的系统提示词，并支持程序目录的agent.md自动注入  |
| 16 | 浏览器朗读（TTS） | Web 镜像内置**浏览器原生语音合成**（Web Speech API）：**默认不启用**，侧栏/面板开关启用（浏览器不支持时强制为关），可选语言/语音（按语音包分组、可 Test），朗读内容二选一或多选（**回合最终文本** / **思考过程** / **工具调用名称** / **文本反馈**），设置存浏览器 `localStorage`（仅当前浏览器、刷新后保留） |

---

## 快速开始

```bash
cd lightagent
go build -o lightagent.exe .     # Windows
go build -o lightagent .         # macOS / Linux

# 首次运行会在“程序所在目录”生成默认 config.json
./lightagent.exe

# 启动时会询问是否恢复已有会话（默认是）；-r 直接恢复，不再询问
./lightagent.exe -r
```

编辑程序目录下的 `config.json`，填入 `openai.api_key`（以及必要的 `api_base` / `model`）后重新运行。

可选：导出内置系统提示词模板并按需修改（程序目录的 `agent.md` 会自动覆盖内置提示词）：

```bash
./lightagent.exe gen-agent-prompt      # 生成 agent.md；已存在则拒绝
./lightagent.exe gen-agent-prompt -f   # 强制覆盖
```

> 开发时用 `go run .` 会让 `os.Executable()` 指向临时目录，可用环境变量
> `LIGHTAGENT_CONFIG` 指定配置文件路径，例如：
> `LIGHTAGENT_CONFIG=./config.json go run .`

---

## 命令行参数

```
lightagent [options]            # 交互式会话
lightagent [options] <command>  # 子命令
```

| 参数 | 作用 |
|------|------|
| `-h`, `--help` | 帮助；`lightagent help <command>` 查看子命令帮助 |
| `-v`, `-V`, `--version` | 版本号（含 commit 信息） |
| `-c`, `--config PATH` | 指定 `config.json`（优先于 `LIGHTAGENT_CONFIG`） |
| `-C`, `--dir PATH` | 先切换到 PATH 再启动（决定 `.lightagent/` 与相对路径） |
| `-r`, `--resume` | 直接恢复上次会话（不再询问） |
| `--session PATH` | 使用指定的会话文件（读写该文件，归档/列表也在其目录） |
| `-p`, `--prompt TEXT` | 非交互跑一个回合后退出；`-` 表示从 stdin 读取提示词 |
| `--json` | 配合 `-p`：输出 JSON 结果（`assistant`/`tools`/`error`/`tokens`/`context_window`） |
| `-q`, `--quiet` | 配合 `-p`：隐藏工具/信息输出（错误仍显示） |
| `--log FILE` | 把渲染输出同时追加写入 FILE |
| `--save` / `--no-save` | 退出时总是保存 / 总是不保存（不再询问） |
| `--print-config` | 打印生效配置（api_key 打码）后退出 |

配置覆盖（**优先级：flag > `LIGHTAGENT_CONFIG` > `config.json` > 内置默认**）：

| 参数 | 覆盖 |
|------|------|
| `--model NAME` | `openai.model` |
| `--api-base URL` | `openai.api_base` |
| `--stream on\|off` | `openai.stream` |
| `--markdown on\|off` | `ui.markdown` |
| `--result on\|off` | 工具/exec 结果是否显示 |
| `--timeout SECONDS` | `openai.timeout_seconds`（空闲超时；长回复不会被总时限截断） |
| `--web-host IP` / `--web-port N` / `--no-web` | `web.host` / `web.port`（0=关闭） |

子命令：

| 命令 | 作用 |
|------|------|
| `gen-agent-prompt [-f]` | 导出内置系统提示词到 `agent.md` |
| `sessions list` | 列出当前会话与归档（`*` 表示当前） |
| `sessions show [--file NAME]` | 打印某个会话的对话 |
| `sessions prune [--keep N\|--all] [--file NAME]` | 删除旧归档（默认保留最新 5 个） |
| `completion bash\|zsh\|powershell` | 输出 shell 补全脚本 |
| `help [command]` | 显示帮助 |

示例：

```bash
lightagent -p "总结一下这个目录" --json      # 脚本化单次问答
lightagent -C ~/proj -r                    # 在指定目录恢复会话
lightagent --print-config | less           # 排查生效配置
lightagent sessions list
source <(lightagent completion bash)       # bash 补全
```

> 未知参数会报错并返回退出码 2（不会静默忽略）。`--` 之后的参数不会被解析为选项。

---

## 配置文件（程序目录 `config.json`）

```jsonc
{
  "openai": {
    "api_base": "https://api.openai.com/v1",
    "api_key": "sk-...",
    "model": "gpt-4o-mini",
    "temperature": 0.0,
    "max_tokens": 4096,
    "timeout_seconds": 120,               // 空闲超时：无数据超过该秒数才中断（0=关闭）
    "stream": true,
    "extra_body": {}                     // 服务商额外请求参数，合并到请求体顶层
  },
  "context": {
    "context_window": 131072,          // 模型上下文窗口（token）
    "summarize_token_percent": 75      // 用量超过该百分比时触发压缩
  },
  "web": {
    "host": "127.0.0.1",                // 监听地址；0.0.0.0 可对局域网开放
    "port": 0,                          // >0 时启动 web 服务；占用则自动 +1 递增
    "password": ""                      // 登录密码（明文保存）；非空即要求登录，盐自动生成
  },
  "tools": {
    "exec":            { "enabled": true, "timeout_seconds": 3600, "wait_seconds": 10 },
    "read_file_lines": { "enabled": true, "max_read_file_size": 32000, "max_read_file_lines": 200 },
    "write_file":      { "enabled": true, "max_lines": 200, "auto_split": true },
    "edit_file":       { "enabled": true },
    "discovery":       { "enabled": false, "mode": "unlock", "ttl": 50, "max_search_results": 10, "min_match_rate": 0.5, "use_bm25": true },
    "mcp": {
      "enabled": false,
      "servers": {
        "filesystem": {
          "enabled": true,
          "command": "npx",
          "args": ["-y", "@modelcontextprotocol/server-filesystem", "."]
        }
      }
    }
  },
  "agent": {
    "max_tool_iterations": 200,
    "system_prompt": ""                 // 留空使用内置默认系统提示词
  },
  "ui": {
    "markdown": true                    // 助手输出按 Markdown 渲染（false 则原样输出）
  }
}
```

* 配置文件位置 = 可执行文件所在目录（可用 `LIGHTAGENT_CONFIG` 覆盖）。
* `web.port > 0` 即开启 web 服务；`web.host` 为空时绑定 `127.0.0.1`；`web.password` 非空时要求**登录**。
* 系统提示词优先级：`agent.md`（程序目录）> `agent.system_prompt` > 内置默认。
* 运行时环境行 `Runtime: <os>/<arch>.`（以及可选的目录清单等）由程序在基础提示词之后自动追加，
  `agent.md` / `agent.system_prompt` 不需要（也不应）写它。
* `agent.md` 支持片段导入：独占一行的 `@include("路径")` 会替换为对应文件（或目录下所有文件）的内容，
  相对路径基于该文件目录，支持嵌套与绝对路径。详见
  [configuration.md](docs/configuration.md#系统提示词覆盖agentmd)。
* `openai.extra_body` 的键会合并进 `/chat/completions` 请求体的**顶层**（可覆盖内置字段）。
* `tools.mcp`：内置纯标准库 MCP 客户端，按 `servers` 连接外部 MCP server（`stdio` / `http` / `sse`）。详见
  [configuration.md](docs/configuration.md#toolsmcpmcp-客户端)。
* `tools.discovery`：MCP 工具发现 / unlock 机制（`tool_search_tool_bm25` → `unlock_tool` → `dynamic_call`）。
  MCP 工具恒为**锁定函数**，永不出现在 `tools` 声明里；系统提示词只注入 1 条全局机制规则 + 每 server 1 条 MCP 全局信息。详见
  [tools.md](docs/tools.md#mcp-工具发现--unlocktoolsdiscovery)。

---

## 目录约定（多实例）

所有运行状态都放在**当前工作目录**下，因此在不同目录启动即可并行运行多个实例互不干扰：

```
<当前工作目录>/
  .lightagent/
    session.json                 # 当前会话（/save 或退出确认保存时写入）
    session-20260912-153000.json # 选择不恢复历史时归档的旧会话（带归档时间）
```

* 会话只在**内存**中维护：对话过程中不会写盘，`/save` 可随时手动保存一次。
* `.lightagent/` 目录**首次保存时才创建**：`--no-save`、一次性 `-p` 运行或退出时选择不保存
  都不会生成该目录。
* 退出（`/exit`、Ctrl+C 空输入、stdin EOF）时会询问 **是否保存**（默认是）。
* 启动时若已有 `session.json`，会询问 **是否恢复历史**（默认是）；选择否时把旧会话
  重命名为 `session-<归档时间>.json` 归档，再开始新会话。
* `lightagent -r`（或 `--resume`）直接恢复，不弹出询问；无文件时提示后开始新会话。
* 非交互（管道）输入无法询问，按默认值处理（恢复 + 保存）。

---

## CLI 命令

| 命令 | 作用 |
|------|------|
| `/help` `/?` | 显示帮助 |
| `/new` | 清空当前会话（仅内存，`/save` 才落盘） |
| `/save` | 立即把当前会话写入 `.lightagent/session.json` |
| `/stop` `/interrupt` | 中断正在运行的回合（模型调用或工具调用） |
| `/compact` | 手动触发一次上下文压缩 |
| `/history` `/context` | 显示消息条数、估算 token 与上下文用量百分比（提示行也实时显示百分比） |
| `/result` | 显示/隐藏工具（exec）结果（`on`/`off`，无参数取反，默认显示） |
| `/markdown` | 切换 Markdown 渲染（`/markdown on` / `/markdown off`，无参数则取反） |
| `/exit` `/quit` | 退出（会先询问是否保存，默认是） |

> 同一套命令也用于 **Web 镜像**：在网页输入框里输入，或点击左侧 **Commands** 卡片（点击即执行）。
> CLI 与网页共用 `internal/slash` 的命令表（名称、别名、参数、说明、分组），两边的 `/help`
> 都由它生成；仅网页侧的命令差异（`/help`、`/markdown` 只作用于当前页面，`/exit` 只在终端生效，
> 未知命令不会发给模型）见 [docs/web.md](docs/web.md)。

**输入方式**：Enter 换行、**Ctrl+J 发送**（Windows 本地也可 Ctrl+Enter；Linux/SSH 下终端支持
kitty 键盘协议时 Ctrl+Enter 同样发送，POSIX 终端上 Alt+Enter 也可以）；Ctrl+U 清空当前输入；**Ctrl+C 优先中断正在运行的回合**，空闲时清空输入，输入为空时退出。

**提示行显示**：**上下文用量标签**（`[ctx 12.3%]`）画在行首、提示标签之前，每收到一次用量报告
就原地刷新，tool 循环进行中同样实时更新；输入为空时在提示标签 `> ` 之后追加灰色
`[Ctrl+J to Send]` 提示，**光标回退到 `> ` 之后**（不会停在提示文字末尾）；回合运行中的
**忙图标与已执行时间**画在 `> ` **之后**、光标之前（青色计时，如 `⠋ 3.4s`，超过一分钟为
`1m05s`，超一小时为 `1h05m`），因此标签不会随动画左右移动；多行消息的续行缩进到首行之下对齐，
**比终端更宽的一行由编辑器自己折行**（续行同样缩进，中文等宽字符整字换行，不会被劈开）。

**工具调用显示**：函数名单独一行高亮（`[tool] 名称`），参数**不缩进**地逐条列出——**单行值**
紧跟参数名（`参数名 - 值`），**多行值**则参数名单独一行、值从下一行开始；值完整显示、用原始内容
（字符串不带引号，嵌套数组/对象为紧凑 JSON；无法解析时回退为原始文本）。配色与网页一致：函数名
黄色加粗、参数名青色加粗、分隔符与值灰色。工具结果以灰色多行块展示，`/result off` 可隐藏。


> Windows 下会尝试请求终端进入 **Win32 input mode**（`CSI ?9001h`），这样 Ctrl+Enter 的修饰键
> 才能穿过 ConPTY 传进来（否则 Ctrl+Enter 与 Enter 无法区分）。该请求仅在 Windows 构建
> ≥ 19041 且 stdout 为真实控制台时发出；设置环境变量 `LIGHTAGENT_SIMPLE_INPUT=1` 可禁用。
>
> Linux / SSH 下会尝试请求终端进入 **kitty 键盘协议**（`CSI > 1 u`，退出时撤销），支持该协议的
> 终端（kitty、foot、ghostty、WezTerm、alacritty、iTerm2、Windows Terminal 等）会把 Ctrl+Enter
> 报成 `CSI 13;5u`，于是 SSH 里也能用 Ctrl+Enter 发送；终端不支持就忽略该请求，用 **Ctrl+J** 发送。
> POSIX 终端上 **Alt+Enter**（meta 前缀 `ESC CR`）同样发送，不需要任何协议；以上请求都受
> `LIGHTAGENT_SIMPLE_INPUT=1` 约束（它只影响请求，不影响 Ctrl+J）。

**插入消息（steering）**：当 Agent 正在执行（思考/调用工具）时，直接输入内容并发送，
该消息会被插入到当前循环中，Agent 在下一步会看到它。若它是在**最后一条回复还在流式输出时**
到达的，本回合会再跑一轮把它并进去；若消息到达时该回合已在收尾（例如工具轮次已用尽、或回合被
中断），Agent 会**立刻为它再开一个回合**，不会把它留到你下次输入。

**显示顺序与提交顺序一致**：`user` 事件在消息**真正进入对话时**才广播，因此这条消息的正式行
总是落在它所打断的那条回复（含该轮的工具行）之后，网页刷新后的回放也在同一位置。在正式发送
之前，消息先以 **pending** 状态显示，且本轮剩余输出始终排在它之前：

* **网页**：消息立刻画成 pending 行（虚线气泡 + `· pending`），本轮后续的行都插在它**之上**；
  Agent 发送时该行**原地**变成普通消息。运行徽标另有 `12.3s · 1 queued` 计数。
* **CLI**：消息作为灰色 `(pending)` 行画在**提示区**（流式预览之下、提示行之上，自动折行）；
  正式发送时该行消失，正式行按事件位置打印（终端无法把已打印的行搬到别处）。没有提示区的
  场合（`-p` 一次性运行、管道输入）打印一行灰色
  `(queued; it joins the conversation after the current reply)`。本终端输入的消息同样由事件流
  绘制（编辑器不再自己回显），所以终端正文里也不会出现消息夹在回复中间的情况。

**中断（interrupt）**：按 **Ctrl+C**（或输入 `/stop`）可停止当前这一回合：

* 正在等模型（思考/流式）时中断 → **丢弃这条记录**：刚提交的用户消息不进入历史，
  下一条消息重新开始；控制台/网页显示 `[interrupted]`。
* 正在执行工具时中断 → 该工具调用反馈 **`interrupted by user`**，已产生的记录保留
  （`exec_command` 的进程会被直接结束）；模型下一回合能看到这次中断。
* 中断后 Agent 回到空闲，**等下一条用户消息再继续**。

---

## Web 镜像

当 `web.port > 0` 时启动。它是 CLI 的**实时网页镜像**：与 CLI 共享同一个 Agent、
同一份会话，CLI 与网页（可同时多个）都能输入，事件通过 **WebSocket** 实时双向同步。

* 访问 `http://<web.host>:<port>/`（默认 `127.0.0.1`；`web.host` 设为 `0.0.0.0` 或内网 IP 可对局域网开放）。
* 输入框与 CLI 一致：**Enter 换行、Ctrl+Enter 发送**（Shift+Enter 也是换行）。
* 网页输入会实时显示到 CLI，CLI 输入也会显示到网页（`user` 事件带 `source` 字段区分来源）；
  `user` 事件在消息进入对话时才广播，所以各端画出的消息行顺序与提交/上下文顺序一致
  （回合运行中提交的消息落在它所打断的回复之后，刷新后回放位置相同）。发送方在这之前把消息
  显示为 **pending**（网页：带 `· pending` 标记的行，本轮输出始终插在它之上，正式发送时原地
  转正；CLI：提示区里的灰色 `(pending)` 行）。
* 顶部右侧实时显示**上下文用量**（token 数与占用百分比，tool 循环进行中也会随每轮更新）。
* 回合运行中顶部显示转圈指示，输入框右侧出现 **Stop** 按钮（点击即中断，同 `/stop`）。
* 助手回复在浏览器里用内嵌的 **marked**（GFM，含表格）+ **DOMPurify** 渲染（`ui.markdown=false`
  时退回纯文本）；两条库文件已随二进制内嵌，无需联网。
* 若设置了 `web.password`，页面会弹出**登录对话框**：浏览器用服务端公布的盐算 `sha256(盐+密码)`
  并只发送该摘要，成功后在 HttpOnly 会话 cookie 上保持登录（勾选 “Stay signed in” 可长期保持，
  并把这枚**加盐摘要**存进浏览器，下次自动静默登录）；页头 **Sign out** 退出。
  密码可在配置编辑器里用 `Set` / `Remove` 设置，**立即生效**（详见 [docs/web.md](docs/web.md)）。
* 端口被占用时自动尝试 `port+1`、`port+2` …（最多 +50），并打印实际端口。
* 工具调用行（`tool_call`）与 CLI 一致：**函数名单独一行高亮**，参数逐条以
  `参数名 - 值` 展示（值用原始内容：字符串不带引号，嵌套数组/对象为紧凑 JSON）。
* 网页里发送 `/result off`（或 `on`）可开关工具（exec）结果输出，与 CLI 共享同一开关。
* **网页与 CLI 共用同一套斜杠命令**：命令表（名称、别名、参数、说明、分组）由 `internal/slash` 统一提供，
  CLI 的 `/help` 与网页的左侧栏、`/help` 都从它生成，因此不会各自漂移。共享状态的命令
  （`/new`、`/save`、`/stop`、`/compact`、`/history`、`/result`）通过事件总线广播，终端与所有网页
  看到同一条反馈；只属于当前页面的命令（`/help` 列表、`/markdown` 渲染开关、拼错的命令）只出现在网页里。
  `/exit` 只在终端生效（网页会提示关闭标签页）；未知命令与 CLI 一样被本地拒绝，不会发给模型。
* 桌面端左侧栏有 **Commands** 命令栏：默认只列 `/help`、`/new`、`/save`、`/stop`（命令表中标为
  `primary` 的那些），其余命令折叠在标题之后 —— **点击标题展开/收起**，标题右侧 `+N` 是折叠数量；
  **点击即执行**，`/result` 与 `/markdown` 显示 on / off 状态，点击切换另一状态。
* 右上角 **⚙**（桌面端侧栏 “Configuration” 卡片）打开配置编辑器，右上角可在 **Form / JSON** 两种模式间切换：
  Form 是分段控件表单（文本框 / 开关 / 滑杆 / 下拉 / 多行文本，按 `config.json` 的键逐项列出，留空即用内置默认），
  JSON 是整份文档原文；两者同步，改动实时写入将提交的文档。保存前两端都会校验（控件行内报错 + 服务端规则），
  **写回文件后重启生效**；`api_key` 打码回显（原样回传即保留原密钥），因此文件始终可启动。
* 右上角 **🔊**（桌面端侧栏 “Read aloud” 卡片）打开**朗读面板**：用浏览器自带的语音合成读出 Agent 的输出。
  **默认不启用**——侧栏卡片里的开关（与面板里的开关是同一个）打开后才朗读；浏览器不支持 Web Speech API 时
  开关强制为关、控件禁用。可选**语言/语音**（按语音包分组，可 `Test`），朗读内容**二选一或多选**——回合的
  最终文本，或 思考过程 / 工具调用（仅函数名）/ 文本反馈；开关与选择存在浏览器 `localStorage`
  （**仅当前浏览器**，刷新保留，不进 `config.json`），刷新页面不会朗读历史（详见 [docs/web.md](docs/web.md)）。

---

## 工具

### `exec_command`
执行脚本，采用「等待后转后台」模型：同步等待最多 `wait_timeout` 秒（默认 10），
若超时则自动转入后台并返回 `session_id`。硬超时由 `run_timeout`（默认取配置）限制。
输出经 ANSI 清理与头尾折叠。

每次调用自成一个**进程树**：结束会话、硬超时、lightagent 退出（含 Ctrl+C / SIGTERM）
都会结束整棵树，不留后台孤儿进程。详见
[进程树与退出](docs/architecture.md#进程树与退出)。

* `language` 选择脚本语言：Windows `ps`（PowerShell）、Unix `sh`（宿主 shell），
  以及系统装了 Python 时的 `python`（**直接以解释器为引擎**，源码经 `-c` 传入）；
  **省略该参数即用宿主 shell**。可选值按主机动态生成，Python 不存在时不出现；
  存在时提示词里带上探测到的版本。
* 子进程 stdio 编码（仅 Windows 有 `use_utf8` 参数，默认取配置 `tools.exec.use_utf8`）：
  `true` 强制脚本引擎使用 UTF-8（PowerShell 前置头 + `PYTHONIOENCODING=utf-8`），Go 不做转码；
  `false` 由 Go 按主机 ANSI 代码页（如 GBK）转换：输出解码为 UTF-8，输入编码为该代码页字节。
  非 Windows 主机始终 UTF-8。

### `manage_session`
管理 `exec_command` 产生的后台会话：`poll`（轮询增量输出，支持长轮询）、
`input`（写入 stdin，支持 `ctrl-c`、`enter` 等控制键）、`kill`（结束整棵进程树）、`list`。

### `read_file_lines`
按行读取文本文件，1 起算的 `start_line` + `max_lines` 分页，输出带行号范围表头与
`[PARTIAL]` / `[TRUNCATED]` / `[END OF FILE]` 标记。CRLF 归一化为 LF。
`encoding` 默认 `utf8`，也可指定字符集标签（`gbk`、`big5`、`shift_jis`、`euc-jp`、
`euc-kr`、`windows-1252`）逐行解码为 UTF-8。

### `write_file`
写入文件，`mode`：`o` 覆盖（默认）、`a` 追加、`c` 仅新建。默认开启自动拆解
（`tools.write_file.auto_split`）：模型一次给出的文本超过 `max_lines` 时，agent 把第一段留在模型自己
那次调用里（历史与后续请求展示的参数也只有第一段），其余分段作为新的 `assistant`/`tool` 往返依次追加，
拼接后与原文逐字节相同（续写段自动用 `mode='a'`，故 `max_lines=200`、原本 500 行会变成 3 个写入往返）；
第一段失败则丢弃其余分段。关闭该开关后恢复旧行为：超限截断，并写入被截断处原有的
换行符（CRLF 保持 `\r\n`），因此续写直接接下一行即可；提示首行给出
`[truncated: N of M lines written; continue with mode='a']` 与续写要求，随后**从截断处原样**列出
后续内容直到 2 行非空行（恰好为空的行输出空行，纯空白行按原样输出并计入）或内容结束，最后以
`...` 收尾。
`encoding`：`utf8`（默认）写文本；`hex`/`base64` 解码二进制载荷；其它标签按字符集编码。
行尾按原样写入，系统不会自动补换行；因此写入的文本不以换行结尾时，成功反馈后面会追加一条
`[no trailing newline: the system never adds one. If the next call appends (mode='a'), start its content
with one newline: \n, or \r\n for a CRLF file.]`（下一步要 `mode='a'` 续写就得在内容最前面自己加一个换行）。

### `edit_file`
按 `mode` 定位 `find` 并在匹配处写入 `content`：`mode='replace'`（默认）用 `content` 替换唯一的字面量 `find`；
`mode='insert'` 把 `content` 插到唯一字面量 `find` 之前（匹配内容原样保留）；
`mode='regex'` 把 `find` 当 RE2 正则，**允许不唯一**，所有匹配都被 `content` 替换（可用 `$1`、`$2` 引用捕获组），
成功反馈给出匹配次数。
`encoding` 支持字符集标签（解码匹配、写回再编码）与 `hex`/`base64` 字节级编辑（`regex` 不可用）；
CRLF 文件按 LF 匹配、写回时恢复 CRLF。

---

## 上下文压缩（统一模式）

`append_instruction` 单一压缩模式，Turn 边界 +
token 预算 + 回合数上限：

* 当估算 token 超过 `context_window * summarize_token_percent%` 时触发。
* 保留窗口从某条 user 消息开始（一个完整 Turn），因此工具调用与其结果不会被切开。
* 保留预算 = `(context_window - max_tokens) / 10`（自动）或 `/ 20`（手动 `/compact`），
  最多保留 3 个（自动）或 2 个（手动）Turn；从新到旧累加，谁先触顶谁停止。
* 把更早的消息按原对话布局发给模型总结（末尾追加压缩指令）：总结请求**原样复用实时请求的请求头**
  （系统提示，含基础提示词以下的能力段落，以及同一份 tools 声明），因此模型总结时所处的环境与产生
  这些消息时一致，且请求前缀与实时请求逐字节相同、服务端提示缓存不会失效；摘要合并进系统提示
  （`# CONVERSATION SUMMARY`）；失败则退化为直接丢弃最旧消息。
* 若压缩后一条消息都不剩（连触发本轮的 user 消息也被压掉），请求前会补一条
  `[engine] Context summarized, continue.` 的 user 消息：聊天模板要求请求里至少有一条
  user 查询，否则接口直接返回 400。
* 压缩后发布带 `summary` 的 `compacted` 事件：CLI 与网页都在**截断处**显示这份摘要
  （CLI 为一个 `[summary]` 块，网页为一条带边框的摘要行），一眼可见详细消息到此为止。
  恢复会话时摘要出现在历史消息之前（网页日志区的第一行），因为切点之前的内容本就只以
  摘要形式保留。
* 总结是一次模型调用（可能较慢），因此压缩**开始前**先发一条 `compacting context:
  summarizing N of M messages` 的 info，CLI 与网页立刻显示「正在压缩」，不会静默等待。
* 可通过 `/compact` 手动触发（手动模式）。

详见 [`docs/architecture.md`](docs/architecture.md)。

---

## 已知限制

* 上下文 token 以上一次接口返回的 `usage.prompt_tokens` 为基准，其后追加的消息为估算值。
