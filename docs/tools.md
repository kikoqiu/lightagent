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
`IsError`；另有 `Silent`（不渲染展示）。注册表 `tools.Registry` 负责按名分派并把 `arguments` 解析为 map。

`ForLLM` 是**工具函数的返回值**：agent 把它**原样**记成该调用的 tool 消息（`role=tool` + `tool_call_id`），
不额外包装、不加信封 —— 工具反馈走的就是 OpenAI 那条正常的 tool 结果通路。

工具由配置开关控制（见 [configuration.md](configuration.md)）：可用的工具有
`exec_command`、`manage_session`、`read_file_lines`、`write_file`、`edit_file`、`webfetch`，
以及需要 `openai.media_types` 与 `tools.upload_media.enabled` **同时成立**时才会出现的
`upload_media`。

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
* 同步等待最多 `wait_timeout` 秒（默认取 `exec.wait_seconds`，10s）：等的是**进程退出**，窗口内写出的
  内容不会提前结束等待。**`exec_command` 就是「启动 + 一次 `poll`」**：窗口结束后的输出处理与
  `manage_session` 的 `poll` 完全一致——把缓冲区当前内容整份交给模型并清空，所以窗口内完成就直接
  返回 `exit_code` 与输出，未完成就转后台返回 `session_id` 并附上当时缓冲区的内容。`max_lines`/
  `max_chars` 只约束**本次调用**返回的量（不跨调用、不按进程生命周期累计）。`max_lines` 的默认值是
  `tools.exec.max_lines`（默认 50）。最大值是 `tools.exec.max_lines_max`（默认 100）。要得更多**自动
  截断到最大值**。重要输出建议重定向到文件再用文件工具读回。
* 硬超时 `run_timeout`（默认取 `exec.timeout_seconds`，3600s）到点强制结束进程；
  `run_timeout=0` 关闭硬超时。
* 每次调用都是一个会话，会话的根进程（宿主 shell 或 Python 解释器）是它**自己进程树的根**：
  结束会话 / 硬超时 / lightagent 退出时整棵树一起结束（[进程树与退出](architecture.md#进程树与退出)）。
  窗口内就等到退出时，退出状态（输出 + `exit_code`）已在回答里，会话当场离开会话池；转后台的会话
  保留到 `poll` 取走它的退出状态为止（没人取走的僵尸最多留 24 小时，见 [`manage_session`](#manage_session)）。
* 输出在会话缓冲里按**终端规则**回放后再给模型：`\r` 只把光标移回第一列（**不擦除**），后续字符逐格
  覆盖，因此更短的重写会留下原文本的尾巴（与终端所见一致，需要擦除就发 `CSI K`）；`\b` 光标左移一格；
  行内 CSI 也照做——`CSI K`（`0`/`1`/`2`，擦除行内）与 `CSI <n> G`/`C`/`D`（列定位、左右移动），这正是
  进度条与 `\r` 搭配的写法（`\r\x1b[K…`）；其余 CSI（颜色、上下移、私有模式等）一律吞掉，不会把转义
  字节交给模型。`\n`（含 CRLF）结束一行，交付的行里不含 `\r`。
* **被就地改写过的行不会逐次刷新地进入上下文**：缓冲区是两次交接之间的窗口，同一窗口内对同一行的
  重画只是在反复覆盖缓冲区里的这一行，因此**只有交接那一刻该行的状态**（连同窗口内写完的每一行）会
  进入上下文。交接 = `exec_command` 的返回与 `manage_session` 的 `poll`：**不论进程是否结束**都把
  缓冲区当前内容整份交出并清空，所以每次调用都能看到进度条当时的状态，下一次调用只看到之后写出的
  内容（同一状态出现在两次调用里是允许的——那是下一次调用自己的事）。从未被就地改写的未完成行
  （如不带换行的提示符）照常立即可见。交出的内容随后按 `max_lines`/`max_chars` 做头尾折叠，上限
  只约束**本次调用**（不跨调用、不按进程生命周期累计）。
* **被用户中断**（回合的中断 / Stop）：`exec_command` 是唯一**不允许继续跑**的调用——整棵进程
  树被强制结束，返回同一份 JSON 契约、`status=interrupted`，`output` 就是进程被终止前已经打印的
  内容（没有输出则为空），`session_id` 为 `null`（没有任何东西留在后台）。恰好在中断时退出的
  进程仍按 `completed` 报告。

参数：

| 参数 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `script` | string | 必填 | 脚本内容，由 `language` 选择引擎解释 |
| `language` | string | 宿主引擎（`ps` / `sh`，即宿主 shell） | **脚本语言**：选择用哪个引擎解释 `script` 文本（是引擎选择，不是调用标签）；**省略/留空即用宿主 shell**。可选值由 schema 的 `enum` 按主机给出（`ps`/`sh`、以及存在时的 `python`）；参数自带的 `description` 是固定文案，各取值含义与 Python 版本在工具描述里 |
| `wait_timeout` | int | `wait_seconds` | 同步等待**进程退出**的秒数（中间有输出也不提前返回） |
| `run_timeout` | int | `exec.timeout_seconds` | 进程总寿命上限（秒），`0` 关闭 |
| `cwd` | string | 进程工作目录 | 子进程工作目录 |
| `use_utf8` | bool | `exec.use_utf8` | **仅 Windows**：`true` 强制脚本引擎使用 UTF-8（PowerShell 前置头 + `PYTHONIOENCODING=utf-8`），Go 不转码；`false` 由 agent 自动按主机 ANSI 代码页解码（其余行为相同，但非本地 ANSI 字符可能无法显示）。见上 |
| `max_lines` | int | `exec.max_lines`（50） | **本次调用**返回的行数上限（头尾折叠；不跨调用累计）。最多可以给到 `exec.max_lines_max`（100），要得更多**自动截断到最大值**。重要输出建议重定向到文件（`> out.txt` / `2>&1`）再用文件工具读回 |
| `max_chars` | int | `30000` | **本次调用**返回的字符上限（不跨调用累计） |

返回（JSON 字符串）：

```json
{
  "status": "completed",   // completed | running | failed | interrupted
  "exit_code": 0,          // 进程已退出时存在（running / interrupted 时为 null）
  "session_id": null,      // status == running 时存在
  "output": "...",
  "truncated": false,
  "total_lines": 1,
  "total_bytes": 5,
  "elapsed_seconds": 0.51,
  "warning": null          // 附注，例如 poll 被中断（进程仍在运行）
}
```

示例（转为后台）：

```json
{ "script": "python -m http.server 8000", "wait_timeout": 3 }
```

示例（宿主引擎与 Python 直接引擎）：

```json
{ "script": "Get-ChildItem -Name", "language": "ps" }
```

```json
{ "script": "import sys\nprint(sys.version)\nprint(2 ** 10)", "language": "python" }
```

---

## `manage_session`

管理 `exec_command` 产生的后台会话（共享同一个会话池）。`exec_command` 可理解为**「启动 + 一次
`poll`」**：窗口结束后的那一步与这里的 `poll` 是同一个操作。

| 参数 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `action` | string | 必填 | `poll` / `input` / `kill` / `list` |
| `session_id` | string | — | 目标会话（`list` 不需要） |
| `data` | string | — | `input` 时写入 stdin 的内容 |
| `wait_timeout` | int | `10` | `poll` 最长等待**进程退出**的秒数（中间有输出也不提前返回） |
| `max_lines` | int | `exec.max_lines` | **本次调用**返回的行数上限（头尾折叠；不跨调用累计）。参数范围**与 `exec_command` 相同**（默认值与最大值都由那里配置，见 [`exec_command` 参数](#exec_command)） |
| `max_chars` | int | `30000` | **本次调用**返回的字符上限（不跨调用累计） |

行为：

* `poll`：等待**进程退出**，最多 `wait_timeout` 秒（与 `exec_command` 的同步等待同一语义：
  窗口内写出的内容不会提前结束等待）。**不论进程是否结束**，都把缓冲区当前内容**整份交出并清空**
  （含被就地重画、尚未定稿的那一行）：仍在运行 → `status=running`（附 `session_id`），已退出 →
  `status=completed` 且带 `exit_code`。因此进度条按每次 poll 当时的状态进入上下文，而两次 poll
  之间被覆盖掉的中间刷新不会出现。`max_lines`/`max_chars` 只约束**本次 poll** 返回的量（不跨
  poll、不按进程生命周期累计）：上限是「这一次交给模型多少」，而不是「这个进程一共交给模型多少」。
  `max_lines` 的**参数范围与 `exec_command` 相同**（默认值与最大值同一处配置，超出自动截断）。
  等待可被**回合中断**取消：中断像 `wait_timeout` 到期一样结束等待，`warning` 说明 poll 被中断
  而进程仍在运行——poll 只是观察者，绝不去动它监视的进程；下一次 poll 继续接着读。
  **把进程的退出状态（退出前写出的输出 + `exit_code`）交出来的那一次 poll 就是该会话的终点**：
  会话随之离开会话池，此后同一个 `session_id` 报 not found。因此**进程退出后仍可 poll**——它就是
  用来取走最后的输出与退出码的（见下面的僵尸说明）。
* `input`：写入 stdin。纯控制键会被翻译：`ctrl-c`、`ctrl-d`、`ctrl-z`、`enter`/`return`、
  `tab`、`esc`、`up`/`down`/`left`/`right`、`backspace`；其余文本原样写入（如需换行请写 `"\n"`）。
  stdio 编码在 `exec_command` 启动该会话时已确定（Windows 上的 `use_utf8`），`manage_session` 不再另行选择。
  进程已经退出时无法写入：回答 `failed`，并提示 poll 该会话取走它的输出。
* `kill`：结束**整棵进程树**（宿主 shell 及其派生的所有子进程；Windows 经 Job Object，Unix 经进程组，见 [architecture.md](architecture.md#进程树与退出)）并**释放会话**，同时把该进程树到此刻为止写出的输出交出来（与 poll 同一套折叠）。两种情形：
  * 进程**仍在运行** → 终止成功（`completed`），`exit_code = -1`（不是自己退出的，没有真实退出码），`output` = 终止说明 + 最后输出的内容；
  * 进程**已经退出** → 没有树可结束，报 `failed`（`exit_code` 为**实际退出码**），`output` = `process already exited with code N` + 最后输出的内容——失败只是说明没杀到东西，退出状态照旧交出、不丢。
  两种情形都会释放会话（此后同一 `session_id` 报 not found）。
* `list`：列出当前会话（id/command/status/pid；已退出的会话带 `exit_code`）。进程已退出但退出状态
  **还没被取走**的会话状态显示为 `zombie`，并附一行说明（poll 即可取走）。

> **僵尸与清理**：会话的进程退出时**不立即清理**——退出前写出的输出与 `exit_code` 还留在池里等着
> 被取走，这正是 `list` 里的 `zombie`（只有「输出 + 退出码等」这些状态，没有进程）。取走（poll 或
> kill）即释放。**24 小时内始终没人 poll 的僵尸自动清理**（`zombieTTL`，见 `internal/tools/session.go`），
> 之后该 `session_id` 报 not found。`exec_command` 在窗口内就等到退出时，退出状态已经写在回答里，
> 会话当场释放、不会留下僵尸；转后台的会话则在 poll 取走退出状态时释放。

> 进程树归属：会话的根进程是它自己进程树的根，因此
> * 会话根进程自行退出时，它留下的后台子进程会被一并清理（会话池不会积累孤儿进程）；
>   若该子进程还占着 stdout/stderr，会话最多再等 2s（`WaitDelay`）收敛输出，然后置为 completed 并把它清掉
>   ——被清掉的是那个留下的**程序**，**会话本身**（输出 + `exit_code`）仍按上面「僵尸与清理」保留到被取走；
> * lightagent 退出（正常退出、Ctrl+C、SIGTERM、终端挂断）时会结束所有仍在运行的会话——`exec_command` 启动的进程不会比 lightagent 活得更久；
> * 仅 Windows 上「本进程被强杀」也能保证清理：kill-on-close 的 Job 句柄随 lightagent 进程关闭，OS 带走整棵树。

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
  与后续请求回传的参数都只有第一段），其余分段由引擎**追加到同一条 assistant 消息上**（作为它额外的
  `tool_call`，排在模型自己那批调用之后，各配一条 `tool_result`），全部分段写完才继续问模型。于是回复
  在模型看来是「一条 assistant + 多个 tool 调用」的形态，preserve thinking 的服务商（如 DeepSeek）能把
  它自己的思考随这条消息一起回传。分段按行边界切分，逐段拼接与原文逐字节相同，续写段一律 `mode='a'`；
  因"段尾保留的换行符"也占一次调用的行额度，除最后一段外每段最多写 `max_lines-1` 行。若第一段就失败
  （如 `mode='c'` 遇到已存在文件），其余分段会被丢弃并发布一条 info，避免文件停在半截。二进制载荷
  （`hex`/`base64`）不受行数限制，不参与拆解。
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
| `find` | string | 必填 | 待定位内容：`replace`/`insert` 为字面量（需唯一）；`regex` 为 RE2 正则；二进制编码时为编码字节串 |
| `content` | string | 必填 | 写在匹配处的内容：`replace`/`regex` 为替换文本（`regex` 可用 `$1`、`$2` 引用捕获组）；`insert` 为插到匹配前方的文本 |
| `mode` | string | `replace` | `replace` 替换唯一字面量匹配；`insert` 把 `content` 插到唯一字面量匹配之前（匹配本身保留）；`regex` 按 RE2 正则替换**所有**匹配 |
| `encoding` | string | `utf8` | `utf8`/字符集标签（文本）；`hex`/`base64`（字节级编辑，禁用 `regex`） |

* `replace`/`insert` 要求 `find` 恰好出现一次，否则报错要求提供更多上下文，并提示改用 `mode='regex'`。
* `regex` **不要求唯一**：匹配多少次就替换多少次，成功反馈给出匹配次数（`regex "…" matched N occurrence(s), all replaced`）。
* `insert` 只把 `content` 插到匹配前方，匹配内容原样保留。
* 文本模式：先按字符集解码为 UTF-8 匹配，写回时再编码；CRLF 文件按 LF 匹配、写回恢复 CRLF。
* 二进制模式（`hex`/`base64`）：`find`/`content` 为编码字节串，唯一匹配；`insert` 插到匹配字节之前。
* JSON 转义生效：`\n` 是换行，`\\n` 是字面反斜杠加 n。
* **不匹配诊断**（仅 `replace`/`insert` 文本模式；二进制编码与 `regex` 没有此诊断）：当 `find` 未找到时，引擎会尝试将 `find` 按行拆分，
  从最长前缀开始递减，找到第一个在全文中**唯一**出现的行前缀（长度 `m`，`m >= 1`）。若找到，
  错误消息会给出：`find` 总行数 `f`、匹配行数 `m`、匹配起始行号 `s`，以及从第 `s+m-1` 行（最后匹配行）
  起的原文内容（最多 `f-m+3` 行，含 2 行冗余以覆盖文件中可能的空行；正文最多 2KB，超出处以
  `... (rest of the excerpt omitted)` 收尾且不会从 UTF-8 字符中间切断），帮助模型定位差异。
  候选前缀为空时跳过：只有 `m == 1` 且 `find` 首行为空才会出现这种情况（空前缀在文中处处匹配，指不出
  差异所在），`find` 超过两行时前缀非空、诊断照常给出。
  若 `find` 为单行或无任何行前缀唯一匹配，则返回通用错误 `` `find` not found in file. Make sure it matches exactly ``。
  两个字面模式的**未找到**与**不唯一**错误各共用一条消息（`insert` 的措辞与 `replace` 完全一致）。

---

## `upload_media`

把一个**本地多媒体文件**读进来并作为附件上传给模型，让模型直接拿到内容（图片、音频、文档本体），
而不是去读它的字节。**只有当 `openai.media_types` 非空且 `tools.upload_media.enabled` 为 `true`
时该工具才存在**（见 [configuration.md](configuration.md#openai-media_types--toolsupload_media多媒体附件)）。

| 参数 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `path` | string | 必填 | 要上传的文件路径。参数的 `description` 里**直接列出生效的可接收类型**（来自 `openai.media_types`） |

* **可接收类型**：由 `openai.media_types` 决定（小写；族名 = `类型/*`）。类型判定**先看扩展名、
  再看文件头**，任一候选被接受即通过。
* **不是可接收类型时报错**（不上传任何内容）：返回形如
  `"x.zip" looks like application/zip, which this model does not accept; upload one of: image/png, application/pdf`，
  模型可据此改走 `exec_command` 转换，或对纯文本改用 `read_file_lines`。
* 其余拒绝情况：路径缺失/空、文件不存在、路径是目录、空文件、超过 `tools.upload_media.max_bytes`
  （默认 20 MiB）——全部只返回错误，**错误结果不带任何附件**。
* 成功时的工具结果分两部分：
  * **文本**（正常 tool 正文）：`Uploaded image/png attachment "shot.png" (12.3 KiB); its content is attached to this tool result.`
  * **附件**（该 tool 消息的 content 数组，紧跟文本之后）：

| 类型 | 载荷 |
|------|------|
| `image/*` | `{"type":"image_url","image_url":{"url":"data:image/png;base64,…"}}` |
| `audio/wav` / `audio/mpeg` 等（可映射到 wav/mp3 的） | `{"type":"input_audio","input_audio":{"data":"<base64>","format":"wav"｜"mp3"}}` |
| 其余（PDF、视频、其他音频等） | `{"type":"file","file":{"filename":"a.pdf","file_data":"data:application/pdf;base64,…"}}` |

  因此该 tool 消息的 `content` 是**数组**（文本 part + 附件 part），这正是 OpenAI 兼容接口承载
  多模态 tool 结果的方式；是否接受 `input_audio`/`file` 这类 part 取决于服务商。
* 附件随消息一起进入历史，因此**后续每一轮请求都会带上**（模型可以反复查看同一张图片）。
* **保存会话只记文件路径，不落 base64**：每个附件 part 记住它读自哪个文件（`path`，绝对路径）和媒体类型
  （`mime`），`store.Save` 用 `llm.ReferenceMedia` 把这两个字段写进会话文件、载荷留空；恢复会话时
  `llm.ResolveMedia` 按该路径把内容读回来，对话因此原样继续。文件已被删除时该 part 仍是「一个带名字的附件」：
  Web 端只显示文件名（图片取不回来），而发给模型的请求里用一句
  `[attachment shot.png is no longer available: …]` 文本 part 代替它（`requestMessages`）。
* `path`/`mime` 从不发给模型：请求里的 media part 只有载荷本身（`requestMessages` 会剥掉这两个字段）。
* 上下文用量估算对每个附件 part 记一个固定成本（不按 base64 长度估算）；真实的
  `usage.prompt_tokens` 是基准：下一次调用返回的真实计数会取代估算，其后的消息再在下限估算上叠加。

---

## `webfetch`

抓取网页并把它简化成 Markdown 交给模型。**只吃网页与文本**：地址返回二进制内容（PDF、图片、
压缩包等）时直接返回**错误结果**（说明是什么类型、并提示用 `exec_command` 下载或转换），
HTTP 路径在读到正文前就按 `Content-Type` 拒绝，浏览器路径在拿到渲染结果后同样检查。

| 参数 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `url` | string | 必填 | 页面地址（`http`/`https`；缺 scheme 时按 `https` 处理） |
| `timeout` | int | `webfetch.timeout_seconds` | 整次抓取的秒数上限（渲染、加载与转换都算在内）。实际取值不会小于 30 秒，详情见下 |
| `method` | string | `fetch_as_md` | 反馈里放什么：`fetch_as_md`（默认，正文 Markdown）/ `save_as_html` / `save_as_md` / `ignore`，见[四种反馈方式](#四种反馈方式method) |
| `invokejs` | string | 空 | 页面加载完、抓取内容**之前**在页面里执行的 JavaScript（**只有浏览器取法支持**，见 [invokejs：在页面里跑脚本](#invokejs在页面里跑脚本)）。给 `http` 模式的工具时直接报参数错误，schema 里也不会出现这个参数 |

* `method` 与 `invokejs` 都不会改变取页面的方式（`mode` 仍然是唯一的取法来源），它们只决定**抓到之后做什么**。
* `invokejs` 是空字符串或省略时等于没给；给了就要求这次抓取真的渲染（`auto` 回退到源码时**不算成功**，见下）。

* `timeout` 参数：工具 schema 的 `default` 就是配置的 `tools.webfetch.timeout_seconds`，描述里直接写出该秒数以及**下限**，
  模型因此知道一次抓取实际能拿到多少时间。**实际超时不会小于 30 秒**（`webFetchTimeoutDefault`）：配置值或参数值更小时
  一律抬到 30 秒；参数缺省或为 `null` 用配置值，`<= 0` 报参数错误。一次抓取的预算涵盖页面加载、渲染与转换。
* 取页面用 `internal/utils/webfetch.go`，取法由 `tools.webfetch.mode` 决定（见
  [configuration.md](configuration.md#tools)）：

  | `mode` | 取法 |
  |--------|------|
  | `auto`（默认） | 用**可见窗口**的浏览器渲染，没有可用浏览器时回退 HTTP 源码 |
  | `chrome-headful` | 必须用可见窗口的浏览器渲染（失败即报错，不退回源码） |
  | `chrome-headless` | 必须用无头浏览器渲染 |
  | `chrome-attached` | 挂到**已在运行**的浏览器（DevTools 端点来自配置），用它自己的登录态与 Cookie |
  | `http` | 只取 HTTP 源码，从不启动浏览器 |

  可见窗口是默认取法的原因：无头浏览器更容易被反爬识别。`chrome-attached` 的端点由
  `attach_address` 给出 —— 一个字符串说清全部：端口 `9222`（即 `127.0.0.1` 上的端口）、
  `192.168.0.5:9223`、`127.0.0.1:9222`，或 `http://…` / `ws://…` 的浏览器地址；为空时用内置的
  `127.0.0.1:9222`。要挂到自己日常用的浏览器上，用 `--remote-debugging-port=9222` 启动它即可
  （端口自选时，可从其 profile 目录的 `DevToolsActivePort` 文件读到实际端口）。
  结果里的 `Method` / `Notes` 说明实际走了哪条路，工具描述按 `mode` 如实陈述取法：**渲染时点名 Chrome**
  （Chromium 系浏览器），并说明"渲染"的含义 —— 真实浏览器加载地址、执行页面 JS、返回它构建出的 DOM
  （因此运行时才生成的内容也能读到），且用的是 agent 自己的 profile；`chrome-attached` 讲的是**概念**：
  连接到了一个**使用者正在使用的浏览器**（只说这一点，不提 DevTools 端点这类技术细节），所以带上它的登录态、
  Cookie 与会话。只取源码的 `http` 模式描述里不会出现浏览器。
* **加载完成之后的等待**（`internal/utils/webfetch.go`）：页面 `readyState` 到 `complete` 后**不立刻抓 DOM** ——
  先等 **1000ms**（宽限：不少站点的首个 AJAX 在 load 之后才发起），再判断**网络空闲**（连续 1000ms 内既没有新请求、
  也没有请求结束；已经发出的请求以它的**结束**事件算作活动），空闲后**再等 1000ms**（让最后一次响应触发的 DOM 更新落地），
  然后才序列化 DOM —— 这是"运行时才生成的内容也能读到"在 SPA / 延迟加载页上的落点。
  判定只看**本页自己的协议会话**：跨域 iframe（OOPIF）有独立会话，其中的请求不计入。
  判定有**上限**：最多再等 4 个空闲窗口、且不少于 5s（`WebFetchNetworkIdleLimitDefault`），并且**永不占用整次抓取的
  最后 2s**（留给 DOM 序列化）；一直有流量的页面（聊天、直播、定时上报）到点就带着**现有 DOM** 返回，并在备注里写明
  「the network was still busy …」，而不是把整次抓取耗光后报错。代价是每次**渲染**抓取多花约 3s。
  页面没能在 `load_timeout` 内加载完（`Loaded=false`）时**不做这个等待**，直接抓现有 DOM；`http` 模式（只取源码）也没有等待。
  内部 API：`WithFetchNetworkIdle(d)` 改这个值（宽限与收尾同用它），`0` 表示完全不等待。
* `auto`、`chrome-headful`、`chrome-headless` 三种**会自己启动浏览器**的模式一律使用 **agent 自己的 profile**
  （工作目录下的 `.lightagent/browser-profile`：**无头模式同样**用它，所以多次抓取之间 Cookie 与登录态是连贯的）。
  不启动浏览器的两种模式**不设 profile**（`BrowserOptions.UserDataDir` 为空）：`chrome-attached` 用的是
  **你正在使用的浏览器**（它的 profile 是你自己的，不该由我们指定），`http` 根本不启动浏览器。
  所以不会打扰你正在使用的浏览器 —— 要用你自己的登录态请选 `chrome-attached`。`auto` 与 `chrome-headful` 会开一个
  **可见窗口**（`auto` 在没有可用浏览器时回退 HTTP 源码并在备注里说明原因；`chrome-headful` 直接报错），
  `chrome-headless` 不开窗口，但用的是**同一个 profile**。
  启动的浏览器在共享池里存活（默认闲置 10 分钟后关闭，程序退出也会关掉它）。
* **`.lightagent/browser-profile` 永不被删除，下次启动直接复用**：它是显式传给浏览器的 profile
  （`BrowserOptions.UserDataDir`），`browserProfileDir` 只负责在缺失时创建、已存在时**原样复用**；utils 里
  **不再存在临时 profile**（`os.MkdirTemp` 创建、随浏览器删除、`ownedProfile`/`removeProfileDir` 这些路径已全部移除）。
  退出路径（`main.go` 的 `defer utils.CloseSharedBrowsers()` 与信号退出注册的 shutdown hook 各调一次）只关掉
  **浏览器进程**，目录连同其中的 Cookie 与登录态原样留在工作目录里，下一次抓取沿用同一个 profile；启动前只清理
  残留（端口文件与"未正常结束"标记，见下一条），**从不删目录**。
  `BrowserOptions` 里**不给目录的无头启动会被直接拒绝**（不会偷偷建临时目录，也不会退回使用者默认 profile），
  只有**带窗口**的启动允许留空目录（此时用使用者的默认 profile，也就是 `chrome-attached` 之外的另一种"用你登录态"的方式）。
  注意：Chrome 只在磁盘 profile 里保留**持久化 Cookie**（带 `Expires`/`Max-Age`），纯**会话 Cookie** 只活在浏览器
  进程的内存里，浏览器一关就没了 —— 所以跨抓取保留登录态的前提是站点发出的 Cookie 本身是持久的；只用会话 Cookie
  的站点请用 `chrome-attached`（借你那个一直开着的浏览器）。
* **浏览器退出与 profile 卫生**（否则残留会让下次抓取一直超时）：
  * 关闭时先通过协议请浏览器**自己退出**（`Browser.close`），最多等 5 秒；只有不退出的才连同进程树**强杀**。
    这样 profile 会留下"正常结束"的状态，下次启动不会再问"是否恢复上次会话"。
  * 启动前还会清掉上次异常退出（超时、被杀、机器断电）留下的残留：profile 里的 DevTools 端口文件
    （指向一个已死的端口，留着会让新启动**一直等这个死端口**直到整次抓取超时）与"未正常结束"标记。
  * 备用保险：读取端口文件时，**探测不通的端口不会被反复等待**，浏览器改写文件后立刻采用新端口。
  * 该 profile 在所有 lightagent 实例之间共享：同时跑两个实例时，后启动的那个会因 profile 被占用而
    起不来（`auto` 会退回 HTTP 源码并在备注里说明原因），这是 Chrome 的 profile 独占所致。

* 其余配置：`browser_path`（渲染用的浏览器可执行文件，空则自动探测）、`user_agent`（两条路径共用的
  UA，空则各用默认）、`max_bytes`（HTTP 源码的字节上限，`0` 用内置 8 MiB）。
* 转换用 `internal/utils/html_converter.go`（`Html2MdConvert`，`BaseURL` 取**重定向后的** `FinalURL`），
  相对链接因此有确定的基准。webfetch 打开两个转换开关：
  * **链接写成站内路径**（`WithRelativeLinks`）：指向页面**同一个站点**（同 scheme + 同 host）的链接
    一律写成根相对路径 —— 路径 + query + fragment，例如 `/docs/other.txt`、`/up.html?x=1#top`；
    指向别的站点、或别的协议（`mailto:` / `tel:`）的链接保持绝对地址，纯 `#fragment` 也保持原样。
    因此 Markdown 里不再重复本站地址，而状态行会写出**这次抓的地址**（`source: <FinalURL>`），
    模型据此就知道相对路径是相对谁。
  * **正文定位**（`WithMainContentSelection`）：转换器按页面自己的声明挑出正文容器（`<main>`、
    `role="main"`、`<article>`，或 `class`/`id` 里带 `content` / `post-content` / `markdown-body` /
    `entry-content` 等名字的块），把标记为**导航/侧栏/页脚/评论/广告/菜单**（`nav`、`sidebar`、
    `header`、`footer`、`comment`、`ad`…）的名字排除在外；候选取文本最多的那个，并在它里面继续收窄
    （包着一层壳的 `<main>` 取里面的 `<article>`；壳自己只多出一个标题时保留壳，标题也一起读）。
    定位结果以 `ContentRegion`（`Located` / `Label` / `StartLine` / `LineCount` / `Markdown`）返回，
    行号是**整篇 Markdown 里**的行号，因此能直接指到落盘文件的位置。
* 工具返回的就是它的返回值（下面这段文本），与其他工具一样作为普通 tool 反馈记录：

  ```
  Conversion succeeded (source: <FinalURL>). Converter warnings (if any): <html_converter 的告警，无则 none>
  [超长时追加：这一段取的是哪一部分、总行数/总字节、落盘文件路径]
  ---

  <Markdown 正文，最多 max_lines 行；被省略的行用标记代替>
  ```

  * 状态行里的 `source:` 是**这次抓取最终的地址**（重定向后），正文里的站内链接就是相对它写的。
  * 告警来自转换器（未知标签、片段包裹等），拼在同一行里。
  * 抓取失败、转换失败、内容不是网页/文本或页面没有可读内容时返回**错误结果**。
  * CLI/网页的展示行是简短一行（地址、取法、行数、字符数、耗时、取页面时的备注），不打印整篇正文。
  * 给了 `invokejs` 且脚本报了值时，`invokejs return info:` 那一段在最前面，状态行与正文在它之后。

### 四种反馈方式（`method`）

* `fetch_as_md`（默认）：就是上面那段 —— 正文 Markdown（最多 `max_lines` 行，见下）。
* `save_as_md`：把**整页的 Markdown**（与默认方式同一份转换结果，因此站内链接同样是根相对路径）写到工作目录的
  `.lightagent/webfetch/<时间戳>.md`，反馈里**只有状态行**：路径、行数与字符数，正文一个字都不进上下文
  （需要时用 `read_file_lines` 分页读）。适合"先把页面存下来、稍后再读"。
* `save_as_html`：把这次抓到的 **HTML 原文**写到 `.lightagent/webfetch/<时间戳>.html`，反馈同样的状态行（路径 + 字符数）。
  浏览器取法存的是**渲染后的 DOM**（含运行时生成的内容），`http` 取法存的是服务器源码 —— 就是给模型看的那份 HTML，
  不做任何链接改写。**这一种方式不做 Markdown 转换**（因此也不会因为"没有可读正文"而失败）。
* `ignore`：什么都不回填，只有状态行（地址、内容类型）。适合"这次调用是为了副作用"（拿脚本的值、预热登录态）。

四种方式都**照常处理 `invokejs`**（脚本的报告永远在反馈最前面），也都可以配 `timeout`；`save_*` 与 `ignore`
不受 `max_lines` 影响（它们本来就不回填正文）。写出来的页面都在工作目录的 **`.lightagent/webfetch/`** 下 ——
一个 fetch 自己的目录，与浏览器 profile（`.lightagent/browser-profile`）和会话文件分开放；文件以抓取时刻命名
（`<时间戳>.md` / `.html`），同名同秒自动加序号，不覆盖。

### `invokejs`：在页面里跑脚本

`invokejs` 是一段 JavaScript，在**页面加载完成之后、抓取内容之前**执行，用来取出"只有页面自己的 JS 才拿得到"
的东西（`document.cookie`、`localStorage` 里的 token、框架内部状态），好让后续命令用**和浏览器一样的身份**去
下载文件、调接口。**只有浏览器取法支持**：`tools.webfetch.mode` 为 `http` 时参数直接报错（schema 里也不会列出
这个参数），`auto` 模式在没有可用浏览器时**不退回源码**，而是把"需要浏览器来跑脚本"作为抓取失败报出来。

执行约定：

* 抓取前会在页面里注入一个全局函数 **`_invokejs_done(value)`**（非枚举属性：`for…in` 与 `Object.keys(window)`
  都看不到它，页面正常遍历自己的全局不会撞上它）。脚本**必须调用它**来结束这次调用。
* 脚本跑在一个 async 函数里，所以可以直接 `await`（例如
  `const r = await fetch('/token'); _invokejs_done(await r.text());`），也可以 `return`（返回不影响等待）。
* **这个模式下不再等网络空闲**（见上文那段等待）：需要什么由脚本自己去要，时间全交给脚本。
* 等待窗口 = **这次抓取剩下的时间**（扣掉留给 DOM 序列化的 2s）；抓取本身没有超时上限时用
  20s（`WebFetchInvokeJSWaitDefault`）。脚本一直不调用时，到点按"等待超时"反馈，页面照常抓回来。
* 报告的值随抓取结果返回（`WebFetchResult.InvokeInfo` / `InvokeNote`），工具把它放在**反馈最前面**：

  ```
  invokejs return info:
  <脚本报告的值>

  <其余原始返回内容>
  ```

  * 值原样回填：**字符串**就是脚本写的那串字符（Cookie 头可以直接给 `exec_command` 用），
    其他值（对象/数组/数字）是它的 JSON。
  * `undefined`、`null` 或空串**什么都不反馈**：连 `invokejs return info:` 这行都不出现 —— 适合"脚本只做注册/预热"
    的用法。
  * 脚本**抛异常**时**立刻**反馈（不等窗口跑完）：
    `invokejs return info: none — the script failed before calling _invokejs_done: <消息>`。
  * 脚本**没调用**时到点反馈：
    `invokejs return info: none — the script did not call _invokejs_done within <N>s`。
  * 值会被截断到 **8 KiB**（并注明已截断），免得一个跑飞的值（整个 DOM、大型 store）把上下文冲掉。
* 展示行会附一句 `invokejs: …`（报了值 / 超时原因），但**值本身只出现在给模型的反馈里**。

### 反馈长度与正文定位（`tools.webfetch.max_lines`）

**默认方式（`method=fetch_as_md`）**的正文默认最多 `max_lines`（`100`）行，避免一次抓取把上下文塞满。
超过限制时**先试着只回填正文**（跳过导航、侧栏、页脚这些框架内容），定位不到正文才退化为回填整篇的中间一段：

* **不超过限制**：正文原样进上下文，没有任何额外说明。
* **超过限制且定位到正文**：回填正文那一段（不到 `max_lines` 行就整段回填），并在被跳过的地方
  用标记说明省了多少行、正文从哪里开始：

  ```
  ...(above: 40 of 812 lines omitted — navigation, sidebars or banner)
  <正文行，最多 max_lines 行>
  ...(the main content continues: 71 of 176 lines omitted, the feedback limit is 100 lines)
  ...(below: 636 of 812 lines omitted — footer, related links or comments)
  ```

  每个标记各自**独占一行**：`above` 之后紧接正文的第一行（不与它挤在同一行），`continues`
  与 `below` 另起一行（上面 `above` 与 `continues` 之间没有空行）。

* **超过限制且定位不到正文**（页面没声明正文，或声明的那块太小/全是链接）：回填**中间 `max_lines` 行**
  （上下各留一半），上下的标记只写省略了多少行：

  ```
  ...(above: 356 of 812 lines omitted)
  <中间 100 行>
  ...(below: 356 of 812 lines omitted)
  ```

* 两种情况下状态行都追加一句"超长"说明：该页共多少行、多少字节，这次取的是哪一段（正文的
  `标签名.类名` 与起始行号，或"中间的 N 行（第 X-Y 行）"），以及整页保存的**文件路径**。
  整页（**Markdown 正文**，与反馈是同一份转换结果）写到**工作目录**的
  `.lightagent/webfetch/<时间戳>.md`（同秒多次抓取自动加序号，不覆盖），
  模型需要全文时用 `read_file_lines` 分页读该文件即可 —— 文件里的行号与状态行报的行号一致，
  不用再转换一次。
* `max_lines` 为**负数**表示不限长度：整页正文照原样回填，也不落盘（`max_lines` 为 `0` 用内置 100）。
* 落盘失败（目录不可写等）时仍然只回填这一段（正文或中间几行）并带上标记，并在状态行里报告失败原因
  —— 抓取结果不会被丢掉。

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

* 用 BM25 在**锁定函数库**（deferred 工具的名称+描述）上排序，最多返回 `max_search_results`（默认 `10`）条。
* **先过匹配率门槛**：`匹配率 = 命中关键词数 / 查询关键词总数`，匹配率低于 `min_match_rate`（默认 `0.5`，即 50%）的函数**不返回**——只沾一个词就冒出来的函数会被挡掉。查询里**重复的关键词按出现次数各算 1 个词**：它既抬高分母，也在每出现一次时多加一份 tf 权重。
* **排序两级**：先按匹配率降序，匹配率相同再按原有 BM25 分数降序。
* **只有查询侧切词**：`query` 按空白切成关键词（剥掉两端标点、转小写），每个关键词再到「名称+描述」的**原文**里做子串包含匹配——文档侧不切词、不建索引，因此 `"set text color"` 能命中 `mcp_office_wps_word_set_text_color` 这类标识符名，中文关键词（`文字`、`颜色`）也能命中中文描述。
* 命中分两档：命中片段两端落在**词边界**（空格/标点/`_`/`-`/驼峰/字母-数字转折/CJK 相邻）算 **全词命中**，黏在 ASCII 字母数字里（`word` ⊂ `password`）算 **片段命中**。两者都算命中（保持宽松召回），但片段命中只按 `0.25` 的权重计入 tf，所以全词命中排在前。
* 已知边界：不做分隔符归一化，`textcolor` 不命中 `text_color`；中文连写短语（`文字颜色`）不是子串，需靠调用方用空格分隔关键词（`文字 颜色`）。
* 只回名称与一行描述，**不返回 schema、不解锁任何东西**；重复搜索不会改变授权。
* 库为空、无匹配、或没有任何函数达到匹配率门槛时返回 “No locked functions found matching the query.”。

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
* **关闭**（lightagent 退出时）：`Manager.Close` 并行关闭每个 server。stdio server 先收到 **stdin EOF**（协议级关闭），最多等 2s 自行退出；超时**只强制结束这个 server 进程本身**（它派生出来的进程不属于我们），此后不再多等，所以 server 不响应也不会拖住 lightagent 退出；`http` 断开空闲连接、`sse` 取消长连接。详见 [进程树与退出](architecture.md#进程树与退出)。

### 工具注册

每个 server 工具被包装为 `tools.Tool`，命名为 `mcp_<server>_<tool>`（小写、非法字符归一为 `_`）；描述与参数 schema 直接取自 server。

* MCP 工具**始终**以 `RegisterDeferred` 作为**锁定函数**注册，进入搜索/解锁体系；**永不出现在模型的 `tools` 声明里**。
* 内置工具（`exec_command` / `manage_session` / `read_file_*` / `write_file` / `edit_file` / `webfetch`）不受此机制影响，始终作为核心工具暴露。

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


