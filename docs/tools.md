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
`exec_command`、`manage_session`、`read_file_lines`、`write_file`、`edit_file`、`webfetch`。

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
  内容不会提前结束等待（与 `manage_session` 的 `poll` 同一语义）。若在窗口内完成，直接返回 `exit_code`
  与输出；否则转入后台并返回 `session_id`，期间累积的输出随该结果一起给出。
* 硬超时 `run_timeout`（默认取 `exec.timeout_seconds`，3600s）到点强制结束进程；
  `run_timeout=0` 关闭硬超时。
* 每次调用都是一个会话，会话的根进程（宿主 shell 或 Python 解释器）是它**自己进程树的根**：
  结束会话 / 硬超时 / lightagent 退出时整棵树一起结束（[进程树与退出](architecture.md#进程树与退出)）。
* 输出在会话缓冲里按**终端规则**回放后再给模型：`\r` 只把光标移回第一列（**不擦除**），后续字符逐格
  覆盖，因此更短的重写会留下原文本的尾巴（与终端所见一致，需要擦除就发 `CSI K`）；`\b` 光标左移一格；
  行内 CSI 也照做——`CSI K`（`0`/`1`/`2`，擦除行内）与 `CSI <n> G`/`C`/`D`（列定位、左右移动），这正是
  进度条与 `\r` 搭配的写法（`\r\x1b[K…`）；其余 CSI（颜色、上下移、私有模式等）一律吞掉，不会把转义
  字节交给模型。`\n`（含 CRLF）结束一行，交付的行里不含 `\r`。
* **被就地改写过的行会扣住不交付**，直到被 `\n` 定稿、或子进程退出（此时交出最后一版）——进度条刷新
  多少次都只算一行、中间状态也不进上下文；从未被就地改写的未完成行（如不带换行的提示符）照常立即交付。
  之后按 `max_lines`/`max_chars` 做头尾折叠。

参数：

| 参数 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `command` | string | 必填 | 脚本内容，由 `language` 解释 |
| `language` | string | 宿主引擎（`ps` / `sh`，即宿主 shell） | 脚本语言；**省略/留空即用宿主 shell**。可选值由 schema 的 `enum` 按主机给出（`ps`/`sh`、以及存在时的 `python`）；参数自带的 `description` 是固定文案，各取值含义与 Python 版本在工具描述里 |
| `wait_timeout` | int | `wait_seconds` | 同步等待**进程退出**的秒数（中间有输出也不提前返回） |
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
| `wait_timeout` | int | `10` | `poll` 最长等待**进程退出**的秒数（中间有输出也不提前返回） |
| `max_lines` | int | `200` | 增量输出行数上限 |
| `max_chars` | int | `30000` | 增量输出字符上限 |

行为：

* `poll`：等待**进程退出**，最多 `wait_timeout` 秒（与 `exec_command` 的同步等待同一语义：
  窗口内写出的内容不会提前结束等待，最后一次性返回**自上次消费以来的增量**）。仍在运行 →
  `status=running`；已退出 → `status=completed` 且带 `exit_code`。被就地改写过的行进不去增量，
  要等它定稿（`\n` 或进程退出）才作为一行出现，因此进度条不会逐次刷新地进入上下文。
* `input`：写入 stdin。纯控制键会被翻译：`ctrl-c`、`ctrl-d`、`ctrl-z`、`enter`/`return`、
  `tab`、`esc`、`up`/`down`/`left`/`right`、`backspace`；其余文本原样写入（如需换行请写 `"\n"`）。
  stdio 编码在 `exec_command` 启动该会话时已确定（Windows 上的 `use_utf8`），`manage_session` 不再另行选择。
* `kill`：结束**整棵进程树**（宿主 shell 及其派生的所有子进程；Windows 经 Job Object，Unix 经进程组，见 [architecture.md](architecture.md#进程树与退出)）并从会话池移除。
* `list`：列出当前会话（id/command/status/pid）。

> 进程树归属：会话的根进程是它自己进程树的根，因此
> * 会话根进程自行退出时，它留下的后台子进程会被一并清理（会话池不会积累孤儿进程）；
>   若该子进程还占着 stdout/stderr，会话最多再等 2s（`WaitDelay`）收敛输出，然后置为 completed 并清理它；
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

---

## `webfetch`

抓取网页并把它简化成 Markdown 交给模型。**只吃网页与文本**：地址返回二进制内容（PDF、图片、
压缩包等）时直接返回**错误结果**（说明是什么类型、并提示用 `exec_command` 下载或转换），
HTTP 路径在读到正文前就按 `Content-Type` 拒绝，浏览器路径在拿到渲染结果后同样检查。

| 参数 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `url` | string | 必填 | 页面地址（`http`/`https`；缺 scheme 时按 `https` 处理） |
| `timeout` | int | `webfetch.timeout_seconds` | 整次抓取的秒数上限（渲染、加载与转换都算在内）。实际取值不会小于 30 秒，详情见下 |

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
* 转换用 `internal/utils/html_converter.go`（`Html2MdConvert`，`BaseURL` 取**重定向后的** `FinalURL`，
  相对链接因此被补成绝对地址）。
* 工具返回的就是它的返回值（下面这段文本），与其他工具一样作为普通 tool 反馈记录：

  ```
  Conversion succeeded. Converter warnings (if any): <html_converter 的告警，无则 none>
  ---

  <Markdown 正文，最多 max_lines 行>
  ```

  * 告警来自转换器（未知标签、片段包裹等），拼在同一行里。
  * 抓取失败、转换失败、内容不是网页/文本或页面没有可读内容时返回**错误结果**。
  * CLI/网页的展示行是简短一行（地址、取法、行数、字符数、耗时、取页面时的备注），不打印整篇正文。

### 反馈长度与落盘（`tools.webfetch.max_lines`）

正文默认最多 `max_lines`（`200`）行，避免一次抓取把上下文塞满：

* 不超过限制：正文原样进上下文，没有任何额外说明。
* 超过限制：只回填**前 N 行**，状态行多一句"超长"说明 —— 该页共多少行、多少字节，
  以及整页保存的**文件路径**；整页（**Markdown 正文**，与反馈是同一份转换结果）写到**工作目录**的
  `.lightagent/webfetch-<时间戳>.md`（同秒多次抓取自动加序号，不覆盖）。
  模型需要全文时用 `read_file_lines` 分页读该文件即可 —— 文件正好接在反馈截断处继续，不用再转换一次。
* `max_lines` 为**负数**表示不限长度：整页正文照原样回填，也不落盘（`max_lines` 为 `0` 用内置 200）。
* 落盘失败（目录不可写等）时仍然只回填前 N 行，并在状态行里报告失败原因 —— 抓取结果不会被丢掉。

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


