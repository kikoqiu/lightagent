# 工具

所有工具实现 `tools.Tool` 接口：

```go
type Tool interface {
    Name() string
    Description() string
    Parameters() map[string]any // JSON Schema
    Execute(ctx context.Context, args map[string]any) *Result
}
```

`Result` 有三个关键字段：`ForLLM`（回填到对话的工具结果）、`ForUser`（CLI/网页展示）、
`IsError`。注册表 `tools.Registry` 负责按名分派并把 `arguments` 解析为 map。

工具由配置开关控制（见 [configuration.md](configuration.md)）：可用的工具有
`exec_command`、`manage_session`、`read_file_lines`、`write_file`、`edit_file`。

---

## `exec_command`

执行脚本，采用「等待后转后台」模型。

* 脚本语言由 `language` 选择，可选值**按当前主机动态生成**（探测不到的引擎不会出现在提示词里）；
  **省略该参数时使用宿主 shell**（即下面的宿主脚本引擎，与旧版行为一致）：
  * 宿主脚本引擎：Windows 为 `ps`（PowerShell 脚本，`pwsh` 优先、回退 `powershell`，
    以 `-NoProfile -NonInteractive -Command` 运行）；非 Windows 为 `sh`（`sh -c`）。
  * `python`：**直接把 Python 解释器当作脚本引擎**（不经 shell，源码以 `-c` 传给解释器，
    因此脚本必须是自包含的 Python 程序）。仅当 PATH 上存在可用的 Python 时才可选，并把探测到的
    **版本**与解释器路径写进工具描述里的 `language` 可选值说明（参数本身的 `description` 是固定
    文案，可选值列表由 schema 的 `enum` 表达）；未安装时该取值**完全不出现**——探测执行一次
    `python -V`（5s 超时），Windows 的 Microsoft Store 占位程序会非零退出且不打印版本，故不会被误认。
  * 其它取值：返回 `unsupported language ...; available: ...`，不执行任何命令。
* 子进程 stdio 编码：**参数 `use_utf8` 仅 Windows 存在**，默认取 `tools.exec.use_utf8`，可按次覆盖。
  * `true`：**强制脚本引擎使用 UTF-8**，Go **不做任何转码**。
    对 PowerShell：脚本前加一段 UTF-8 前置头——把 `[Console]::InputEncoding`、`[Console]::OutputEncoding`
    与 `$OutputEncoding` 设为无 BOM 的 UTF-8（前两者经由 `SetConsoleCP`/`SetConsoleOutputCP` 让子进程
    继承的控制台代码页也切到 UTF-8）；对 Python（`language=python` 时的直接引擎，以及脚本内部调用的
    python）：向子进程注入 `PYTHONIOENCODING=utf-8`。子进程 stdout/stderr 字节原样返回。
  * `false`：不注入任何前置语句与变量，改由 **agent（Go）按主机 ANSI 代码页自动解码**：
    stdout/stderr 从 `GetACP` 代码页（如 CP936→GBK）解码为 UTF-8，写入 stdin 的文本编码为该代码页字节。
    `language=python` 时 Python 会按主机代码页写 stdout，正好被解码。除此之外一切行为相同；但
    **非本地 ANSI 字符可能无法显示**——主机代码页表外的字符（例如 Latin-1 主机上的中文、GBK 主机上
    无法映射的符号）只能得到替换字符或乱码。
  * 非 Windows 主机**没有** `use_utf8` 参数（也没有可回退的 ANSI 代码页）：始终 UTF-8，
    仅额外注入 `PYTHONIOENCODING=utf-8`。
* 同步等待最多 `wait_timeout` 秒（默认取 `exec.wait_seconds`，10s）。若在窗口内完成，
  直接返回 `exit_code` 与输出；否则转入后台并返回 `session_id`。
* 硬超时 `run_timeout`（默认取 `exec.timeout_seconds`，3600s）到点强制结束进程；
  `run_timeout=0` 关闭硬超时。
* 输出经 ANSI 清理、CR 重放（进度条折叠）后按 `max_lines`/`max_chars` 做头尾折叠。

参数：

| 参数 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `command` | string | 必填 | 脚本内容，由 `language` 解释 |
| `language` | string | 宿主引擎（`ps` / `sh`，即宿主 shell） | 脚本语言；**省略/留空即用宿主 shell**。可选值由 schema 的 `enum` 按主机给出（`ps`/`sh`、以及存在时的 `python`）；参数自带的 `description` 是固定文案，各取值含义与 Python 版本在工具描述里 |
| `wait_timeout` | int | `wait_seconds` | 同步等待秒数 |
| `run_timeout` | int | `exec.timeout_seconds` | 进程总寿命上限（秒），`0` 关闭 |
| `cwd` | string | 进程工作目录 | 子进程工作目录 |
| `use_utf8` | bool | `exec.use_utf8` | **仅 Windows**：`true` 强制脚本引擎使用 UTF-8（PowerShell 前置头 + `PYTHONIOENCODING=utf-8`），Go 不转码；`false` 由 agent 自动按主机 ANSI 代码页解码（其余行为相同，但非本地 ANSI 字符可能无法显示）。见上 |
| `max_lines` | int | `200` | 输出行数上限 |
| `max_chars` | int | `30000` | 输出字符上限 |

返回（JSON 字符串）：

```json
{
  "status": "completed",   // completed | running | failed
  "exit_code": 0,          // status != running 时存在
  "session_id": null,      // status == running 时存在
  "output": "...",
  "truncated": false,
  "total_lines": 1,
  "total_bytes": 5,
  "elapsed_seconds": 0.51,
  "warning": null
}
```

示例（转为后台）：

```json
{ "command": "python -m http.server 8000", "wait_timeout": 3 }
```

示例（宿主引擎与 Python 直接引擎）：

```json
{ "command": "Get-ChildItem -Name", "language": "ps" }
```

```json
{ "command": "import sys\nprint(sys.version)\nprint(2 ** 10)", "language": "python" }
```

---

## `manage_session`

管理 `exec_command` 产生的后台会话（共享同一个会话池）。

| 参数 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `action` | string | 必填 | `poll` / `input` / `kill` / `list` |
| `session_id` | string | — | 目标会话（`list` 不需要） |
| `data` | string | — | `input` 时写入 stdin 的内容 |
| `wait_timeout` | int | `10` | `poll` 最长等待新输出/退出的秒数 |
| `max_lines` | int | `200` | 增量输出行数上限 |
| `max_chars` | int | `30000` | 增量输出字符上限 |

行为：

* `poll`：先长轮询等待（有新输出或进程退出即返回），再返回**自上次消费以来的增量**。
  仍在运行 → `status=running`；已退出 → `status=completed` 且带 `exit_code`。
* `input`：写入 stdin。纯控制键会被翻译：`ctrl-c`、`ctrl-d`、`ctrl-z`、`enter`/`return`、
  `tab`、`esc`、`up`/`down`/`left`/`right`、`backspace`；其余文本原样写入（如需换行请写 `"\n"`）。
  stdio 编码在 `exec_command` 启动该会话时已确定（Windows 上的 `use_utf8`），`manage_session` 不再另行选择。
* `kill`：结束进程并从会话池移除。
* `list`：列出当前会话（id/command/status/pid）。

> 已知限制：`kill` 通过结束该进程实现，不保证清理其派生的子进程树。

---

## `read_file_lines`

按行读取文本文件，适合源码、Markdown、日志、配置。

| 参数 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `path` | string | 必填 | 文件路径 |
| `start_line` | int | `1` | 1 起算、包含的首行 |
| `max_lines` | int | `max_read_file_lines` | 本次最多返回行数 |
| `encoding` | string | `utf8` | 文本编码或字符集标签 |

* 输出无行号；表头给出该窗口的首行文件行号：`[file: <base> | lines X-Y | first row below = file line X]`。
* CRLF 归一化为 LF。
* 页脚标记：`[PARTIAL - ...]`（还有内容）、`[TRUNCATED - ...]`（字节预算用尽）、
  `[END OF FILE - no further content.]`。
* 续读：`start_line = 上次最后一行 + 1`（页脚会给出该值）。
* `encoding`：`utf8`（默认，二进制会被拒绝）；或字符集标签（`gbk`、`big5`、`shift_jis`、
  `euc-jp`、`euc-kr`、`windows-1252`）逐行解码为 UTF-8。`hex`/`base64` 不适用（逐行无意义）。

---

## `write_file`

| 参数 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `path` | string | 必填 | 文件路径 |
| `content` | string | 必填 | 写入内容 |
| `mode` | string | `o` | `o` 覆盖、`a` 追加、`c` 仅新建（互斥） |
| `encoding` | string | `utf8` | `utf8` 文本；`hex`/`base64` 二进制载荷；其它为字符集标签 |

* **自动拆解（`tools.write_file.auto_split`，默认开启）**：模型一次给出的文本超过 `max_lines` 时，
  agent 不再把超限提示交回模型，而是自动拆成 n 次写入——模型自己那次调用只保留第一段（历史、前端展示
  与后续请求回传的参数都只有第一段），其余分段由引擎**在本轮工具结果之后**作为独立往返追加：
  `assistant(write_file 第 k 段)` → `tool(第 k 段写入结果)`，全部分段写完才继续问模型。分段按行边界
  切分，逐段拼接与原文逐字节相同，续写段一律 `mode='a'`；因"段尾保留的换行符"也占一次调用的行额度，
  除最后一段外每段最多写 `max_lines-1` 行。若第一段就失败（如 `mode='c'` 遇到已存在文件），其余分段
  会被丢弃并发布一条 info，避免文件停在半截。二进制载荷（`hex`/`base64`）不受行数限制，不参与拆解。
* 关闭自动拆解后恢复下面的截断行为：文本内容按 `write_file.max_lines` 截断（超出丢弃并提示用
  `mode='a'` 续写）；**截断时会写入被截断处
  原有的换行符**（源内容为 CRLF 时保留为 `\r\n`，即最末尾那行的换行不会被吞掉），所以续写应直接从
  下一行内容开始，不要额外加前导换行。截断提示固定为
  `[truncated: N of M lines written; continue with mode='a']` + 两条续写要求（不要少写、分多次
  `mode='a'` 续写且最后一行是否带换行由模型按原文自行保留）+ `The exact truncated lines are displayed as follows …`，
  随后**从截断处原样**列出后续内容直到 **2 行非空行**（恰好为空的行输出为空行，纯空白行按原样
  输出并计入这 2 行）或内容结束，最后以 `...` 收尾；二进制载荷不受行数限制。
* 行尾（LF/CRLF）按原样写入，系统不会自动补换行（唯一例外：截断时补回原文本身就存在的那个换行符）。
  写入的文本**不以换行结尾**时，成功反馈（`File created/overwritten` 或 `Appended to`）之后追加一条
  `[no trailing newline: the system never adds one. If the next call appends (mode='a'), start its content
  with one newline: \n, or \r\n for a CRLF file.]` —— 文件就停在最后一个字符处，下一步若要 `mode='a'`
  续写，**内容最前面要自己加一个换行**（LF 文件 `\n`，CRLF 文件 `\r\n`）。空内容、二进制载荷
  （`hex`/`base64`）与截断写入不带这条提示。
* `mode='c'` 对已存在文件报错；`mode='a'` 对不存在文件会创建并说明。

---

## `edit_file`

| 参数 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `path` | string | 必填 | 文件路径 |
| `old_text` | string | 必填 | 待替换文本（字面量需唯一；`regex=true` 时为 RE2 正则；二进制时为编码字节串） |
| `new_text` | string | 必填 | 替换文本；`regex=true` 可用 `$1`、`$2` 引用捕获组 |
| `regex` | bool | `false` | 按 RE2 正则替换所有匹配 |
| `encoding` | string | `utf8` | `utf8`/字符集标签（文本）；`hex`/`base64`（字节级替换，禁用 regex） |

* 字面量模式要求 `old_text` 恰好出现一次，否则报错要求提供更多上下文。
* 文本模式：先按字符集解码为 UTF-8 匹配，写回时再编码；CRLF 文件按 LF 匹配、写回恢复 CRLF。
* 二进制模式：`old_text`/`new_text` 为 `hex`/`base64` 字节串，唯一替换。
* JSON 转义生效：`\n` 是换行，`\\n` 是字面反斜杠加 n。

---

## MCP 工具发现 / unlock（`tools.discovery`）

MCP unlock 发现机制。**锁定函数**由宿主用 `Registry.RegisterDeferred` 注册：它们不进入模型声明的 `tools` 数组、也不出现在系统提示词里，只有拿到有效授权（grant）才允许执行。模型按 「搜索 → 解锁 → 间接调用」三步使用。开启 `tools.discovery.enabled` 后提供以下三个控制面工具，并在系统提示词追加 **Tool Discovery & Unlock** 规则（详见 [configuration.md](configuration.md#toolsdiscoverymcp-工具发现--unlock)）。

工作流：

```
llm:  tool_search_tool_bm25(query="create a github issue")
tool: 返回匹配函数的精确名称与一行描述（仅发现，不解锁）
llm:  unlock_tool(name="mcp_github_create_issue")
tool: <unlock_schema id="mcp_github_create_issue"><tools>…完整 schema…</tools></unlock_schema>
llm:  dynamic_call(name="mcp_github_create_issue", arguments={"title": "Fix typo"})
tool: <真实执行结果>
```

### `tool_search_tool_bm25`

| 参数 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `query` | string | 必填 | 用自然语言描述你需要的动作 |

* 用 BM25 在**锁定函数库**（deferred 工具的名称+描述）上排序，最多返回 `max_search_results` 条。
* **只有查询侧切词**：`query` 按空白切成关键词（剥掉两端标点、转小写），每个关键词再到「名称+描述」的**原文**里做子串包含匹配——文档侧不切词、不建索引，因此 `"set text color"` 能命中 `mcp_office_wps_word_set_text_color` 这类标识符名，中文关键词（`文字`、`颜色`）也能命中中文描述。
* 命中分两档：命中片段两端落在**词边界**（空格/标点/`_`/`-`/驼峰/字母-数字转折/CJK 相邻）算 **全词命中**，黏在 ASCII 字母数字里（`word` ⊂ `password`）算 **片段命中**。两者都算命中（保持宽松召回），但片段命中只按 `0.25` 的权重计入 tf，所以全词命中排在前。
* 已知边界：不做分隔符归一化，`textcolor` 不命中 `text_color`；中文连写短语（`文字颜色`）不是子串，需靠调用方用空格分隔关键词（`文字 颜色`）。
* 只回名称与一行描述，**不返回 schema、不解锁任何东西**；重复搜索不会改变授权。
* 无匹配或库为空时返回 “No locked functions found matching the query.”。

### `unlock_tool`

| 参数 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `name` | string | 必填 | 搜索返回的锁定函数精确名称 |

* 授予 `ttl` 个**工具执行轮**的临时执行授权（每轮结束递减），并把该函数的完整定义以规范化 XML `<tools>` 块包在 `<unlock_schema id="…">` 记录里返回。
* 若该 schema 记录仍在**当前有效（未压缩）上下文**里，只刷新授权并提示“之前已发过”，不重复下发 schema；被压缩淘汰后再次 `unlock_tool` 会重新下发。
* 对核心工具调用只提示“无需解锁”；未知名报错。

### `dynamic_call`

| 参数 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `name` | string | 必填 | 已解锁的锁定函数精确名称 |
| `arguments` | object | `{}` | 依据 `unlock_tool` 下发的 `<parameters>` schema 构造的 JSON 对象 |

* 经注册表的授权闸门转发执行，效果与直接调用一致（部分模型模板会丢弃不在 `tools` 声明列表里的 `tool_calls`，故需间接调用）。
* 未激活（无授权）时返回 `tool is locked` 错误，指引先 `unlock_tool` —— 形成自愈闭环；授权过期后函数仍可被搜索，重新 `unlock_tool` 即可。
* 不会转发到控制面工具（`unlock_tool` / `dynamic_call`）或核心工具。

---

## MCP 客户端

`internal/mcp` 是一个**纯标准库**实现的 MCP 客户端（JSON-RPC 2.0），把外部 MCP server 的工具接入 lightagent。server 在 [`tools.mcp`](configuration.md#toolsmcpmcp-客户端) 中配置，启动时连接并加载工具。

### 连接

* 启动时（`runSession`）对每个 `enabled` 的 server 建立连接：`initialize` 握手 → `notifications/initialized` → `tools/list`（自动跟随 `nextCursor` 分页）。
* 单个 server 失败不影响其他 server，错误在启动时以 `mcp: …` 打印。
* 传输：
  * `stdio`：启动子进程，按行分隔 JSON-RPC（Windows 上 `npx`/`.cmd` 会自动经 `cmd /c` 启动）。
  * `http` / `streamable-http`：每个 JSON-RPC 消息一个 POST；响应可为 JSON 或 SSE；服务端返回的 `Mcp-Session-Id` 会在后续请求回传。
  * `sse`：旧版长连接事件流；从 `endpoint` 事件取得消息地址，响应经 `message` 事件送回。

### 工具注册

每个 server 工具被包装为 `tools.Tool`，命名为 `mcp_<server>_<tool>`（小写、非法字符归一为 `_`）；描述与参数 schema 直接取自 server。

* MCP 工具**始终**以 `RegisterDeferred` 作为**锁定函数**注册，进入搜索/解锁体系；**永不出现在模型的 `tools` 声明里**。
* 内置工具（`exec_command` / `manage_session` / `read_file_*` / `write_file` / `edit_file`）不受此机制影响，始终作为核心工具暴露。

### 系统提示词注入

启动时只注入两类**机制说明**（不含任何函数名，保证前缀稳定）：

* **全局 unlock 规则**：整段提示词里**只出现 1 次**，说明 search → `unlock_tool` → `dynamic_call` 的用法（`agent.ToolUnlockRule()`）。
* **MCP 全局信息**：每个连上的 server 一条，含三部分——
  * 原项目句式：`MCP server \`<配置键名>\` is connected. It contributes N tool(s), currently registered as locked tools; discover … unlock … dynamic_call.`
  * **server 返回的信息**（来自 MCP `initialize` 结果）：`Reported by: name "…", title "…", version …`（有则显示）。
  * **server instructions**（`initialize` 的 `instructions`，有则显示）：`Server instructions: …`。

两类都只在**存在锁定函数**时出现；没有锁定函数时不会往上下文插入任何多余内容。

### 调用与结果

工具调用经 `tools/call` 发往对应 server；结果 `content` 数组按类型拍平为文本：`text` 原样拼接，`image`/`audio`/`resource` 折叠为占位说明，缺内容时回退到 `structuredContent` 的 JSON。server 返回 `isError=true` 时工具结果标记为错误。


