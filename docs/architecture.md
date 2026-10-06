# 架构

lightagent 是一个单进程、多协程的微型 Agent。除 `golang.org/x/text`（字符集转换）外
只依赖 Go 标准库。

## 模块与职责

| 包 | 职责 |
|----|------|
| `internal/config` | 读取程序目录的 `config.json` 与可选 `agent.md`，提供默认值与校验 |
| `internal/llm` | OpenAI 兼容的 `/chat/completions` 客户端：流式 SSE 与非流式解析（含 `reasoning_content` / `reasoning` 思考透出）、工具调用聚合、`extra_body` 合并、空闲超时看门狗 |
| `internal/agent` | 回合循环（tool loop）、steering、事件总线、上下文压缩、系统提示词 |
| `internal/tools` | 工具接口与注册表（含 MCP unlock 的 deferred/grant 控制面、BM25 发现搜索、`unlock_tool`、`dynamic_call`）、命令执行引擎与会话池（进程退出后留作**僵尸**：输出 + `exit_code` 保留到被 poll/kill 取走，没人取走则 24h 后清理）、文件工具、网页抓取（`webfetch`：utils 取页面 + 转 Markdown，取法/浏览器/UA/字节上限/反馈行数来自 `tools.webfetch`；正文站内链接写成根相对路径，超长页面优先只回填**定位到的正文**、否则回填中间若干行，省略处与落盘路径写进反馈；`method` 决定反馈放什么（含落盘、只要状态行），`invokejs` 的脚本报告放在反馈最前面）、字符集编解码 |
| `internal/utils` | HTML → Markdown 转换器（纯标准库 + `x/net/html`：标题/段落/列表/表格/代码块/公式/布局启发式，**可选把站内链接写成根相对路径**与**正文定位**（`<article>`/`<main>`/`class=main` 等容器，可按行号报告））；网页抓取（HTTP 源码 / 可见 / 无头 / 挂载浏览器四种取法 + 字符集、Cookie 与 profile，**在渲染好的页面里执行调用方给的脚本**（`Page.InvokeJS`：注入非枚举的 `_invokejs_done`，等待其回调或脚本失败））；浏览器池与 stealth |
| `internal/mcp` | MCP 客户端（纯标准库）：JSON-RPC 2.0 over stdio / Streamable HTTP / HTTP+SSE；配置驱动的 `Manager` 与 `tools.Tool` 适配器 |
| `internal/proc` | 子进程启动与停止：**树模式**（`Start`，Windows 用 Job Object「kill-on-close」、Unix 用独立进程组 `SIGTERM`→`SIGKILL`，exec 会话用）与**单进程模式**（`StartProcess`，只停止自己启动的那个进程，stdio MCP server 用）；`Shutdown` 在主进程退出或收到信号时按各自模式停止所有仍存活的进程 |
| `internal/store` | 多会话持久化（会话文件在 `CWD/.lightagent/sessions/`，当前会话为 `session.json`） |
| `internal/lock` | 目录锁（纯 `syscall`）：`<状态目录>/.lock` 上 `flock`（Unix）/ `LockFileEx`（Windows），使一个目录只运行一个实例；释放时删锁文件，目录里再没别的就删目录 |
| `internal/cli` | 彩色 REPL、斜杠命令、Markdown 流式渲染（未完成行作为预览绘制在提示符上方，按终端宽度折行、最多 8 行，因此超出首行的文本也边收边显示）、提示区原地逐行重绘（不整块擦除，老式 Windows 控制台才不会闪屏）、`[thinking]` 思考流式块（同样应用 Markdown）、异步渲染事件 |
| `internal/slash` | 斜杠命令表（名称 / 别名 / 参数 / 说明 / 网页是否常显）：CLI 的 `/help`、网页的 `/help` 与左侧命令栏都由此生成；同时提供命令解析（全角斜杠、别名归一）、on/off 参数解析与 `/history` 用量文案 |
| `internal/web` | HTTP + WebSocket 实时镜像；stdlib 实现 RFC6455；内嵌 marked + DOMPurify 供浏览器渲染 Markdown；出站流式增量按 50ms 合帧（`web.go`），后台节流、手机隐藏超时后停表断连（`app.js`） |
| `internal/markdown` | 无依赖的 Markdown → ANSI 渲染（CLI 用），按行流式输出并暴露未完成行（`Pending`）供预览 |
| `internal/termcolor` | ANSI 彩色封装（检测到终端支持才着色） |
| `internal/textwidth` | 终端列宽测量：东亚宽字符算 2 列、制表符算最宽跳位、控制符算 0；CLI 的提示区折行与 `/list` 的列对齐共用它，含按列右补空格的 `PadRight` |

## Agent 回合循环

入口是 `Agent.Submit(text)`：

1. `Submit` 用互斥锁判断当前是否忙碌。
   * 空闲：置 `busy=true`，发布 `user` 事件，启动 `runLoop` 协程。
   * 忙碌：把消息推入 `steerCh`（steering），并发布 `user` 事件。
2. `runLoop` 执行：
   1. 追加第一条 user 消息并广播一次 `usage`（前端立刻反映新消息带来的用量）。
   2. 每轮开始时 `drainSteering()`：把 `steerCh` 中的消息按序追加为 user 消息，**并在此刻广播
      `user` 事件**——消息行出现在它真正进入对话的位置（它所打断的那条回复之后），因此各前端
      画出的顺序、以及 web 回放缓冲记录的顺序，都与模型看到的消息顺序一致。
   3. `compactIfNeeded()`：请求用量超阈值则压缩（见下）。
   4. `callLLM()`：构造 `[system, ...history]`，附带工具定义，流式调用；模型可见回答的增量
      文本发布为 `assistant_delta`；若服务商返回思考内容（`reasoning_content` / `reasoning`），
      其增量先发布为 `reasoning_delta`。同一条回复的各事件共用同一个**回复开始时刻**（第一个
      流式块到达时记下），供前端画出消息旁的时间戳（见[事件总线](#事件总线)）。
      * **调用失败**：被用户中断的走下面的「中断」分支（服务商已经流出的那部分回复会保留）；
        **服务商以「上下文超限」拒绝**
        （`llm.IsContextLengthError`，见[溢出恢复](#溢出恢复provider-拒绝后回退--摘要--重发)）时
        回退本轮消息、摘要后重发同一条消息（次数有上限）；其余错误仍以 `error` 结束回合。
   5. 追加 assistant 消息（含组装后的思考 `reasoning_content`，会随后续请求回传）并广播
      `usage`。模型答复在返回前会做**完整性校验**（`llm.validateResponse`），不合格的响应直接
      报错并结束本回合，不会写入历史：
      * 工具调用的函数名为空，或参数不是合法 JSON —— 说明服务端把响应截断在调用中途
        （即便它最后发的是 `finish_reason=stop`），按「半条工具调用」报错；
      * `finish_reason=length` 且已产生 tool_calls —— 截断可能吃掉调用，提示提高
        当前接口的 `providers[].max_tokens`；
      * `finish_reason=content_filter` —— 答复不完整；
      * SSE 帧以 `{` / `[` 开头却无法解析 —— 帧被截断，静默丢弃会导致内容/工具片段丢失，故报错
        （非 JSON 的心跳帧仍照旧忽略）。
      参数既接受 OpenAI 的字符串分片，也接受少数服务端直接给出的 JSON 值
      （`{"arguments":{}}`）或省略/`null`（无参调用），都先拼成同一份文本再校验。
      无 tool_calls 的 `finish_reason=length` 不属于错误，交给下面的自动续跑处理。
      以上「不完整答复」统一返回 `llm.IncompleteResponseError`（可用 `llm.IsIncompleteResponse`
      判定），目前一律以 `error` 结束回合；`runTurn` 的错误分支处已标注**预留的恢复挂载点**
      ——将来若要「把解析错误回喂模型、让它重发调用」，就在该处按这个判定分支（策略未定，暂不启用）。
   6. 无 `tool_calls` 时：**回复只有思考**（可见正文 trim 后为空、无工具调用，**无论 `finish_reason` 是正常结束、`length` 截断、还是没有给出**）走「只有思考」通路——`agent.include_only_think` 决定是否记入历史，`agent.continue_only_think`（仅前者为真时生效）决定是否记一条 `info`（`the reply carried only thinking; asking the model again`）后带着这段思考再问一次模型，默认都开。其余 `finish_reason=length`（被 `max_tokens` 截断但**带了正文**）→ 自动续跑：不追加用户消息，直接保留该 assistant 消息进入下一轮。**这两种自动续跑（只有思考 / `length` 截断）共用同一个连续计数**（`maxConsecutiveAutoContinues`=3）：连续 3 次即报 `error` 并结束回合，中途出现工具轮次会把计数清零。若此刻 `steerCh` 里还有消息（流式过程中刚插入的），本回合继续下一轮把它并入上下文。
   7. 逐个执行工具，发布 `tool_call` / `tool_result`，把结果作为 `tool` 消息追加；本轮结束后
      广播 `usage`（工具结果同样占用上下文），回到 2。tool 消息的正文就是工具函数的返回值
      （`res.ForLLM`），不加任何包装/信封；被中断的调用也在这条通路上记一条
      `interrupted by user before the call started`（见下面的「中断」）。
      * **带附件的工具结果**（目前只有 `upload_media`）：`res.Media` 非空时该 tool 消息的
        `content` 写成**数组** —— 文本 part（`ForLLM`）在前，附件 part 在后；不带附件时仍是普通
        字符串，`llm.Message` 的编解码负责这个形态（会话文件里的 media part 同一形态，但**只写
        `path`/`mime`，不写载荷**，见[持久化](#持久化)）。
        用户消息同理：Web 附带的文件作为该用户消息的 media 一起发送，用户事件（`user`）另带一份
        **附件描述**（`attachments`：名字 / 媒体类型 / 可取的 URL），前端据此在消息行里把图片画成图片。
      * **write_file 自动拆解**（`tools.write_file.auto_split`，默认开启）：若某次 `write_file` 的
        文本载荷超过 `write_file.max_lines`，该调用的参数在落库前先被改写成第一段（历史、前端展示与
        后续请求回传的都是实际执行的参数），其余分段追加为**同一条 assistant 消息上的额外 `tool_call`**
        （排在模型自己那批调用之后，续写段 `mode='a'`，各配一条 `tool_result`），全部写完才回到 2
        继续问模型；于是这条回复在模型看来是「一条 assistant + 多个 tool 调用」的形态，preserve
        thinking 的服务商（如 DeepSeek）也就能把它自己的思考随这条消息一起回传。第一段失败则丢弃其余
        分段并发布 `info`。超限的写**就是先被展开成多条写**：这些分段与同批其他调用没有任何区别——
        不管它们是模型一次发出的，还是被展开出来的——中断时同样不特殊处理（在跑的那段反馈真实结果，
        其余未开始的段记为 `interrupted by user`，见下面的「中断」）。
   8. 达到 `max_tool_iterations` 时发布 `info` 并结束。
3. 结束时置 `busy=false`、保存会话、发布 `turn_done`；随后若 `steerCh` 仍非空（消息在最后一轮
   **之后**才到，已无轮次可并入），则由 `startSteeringTurn()` 立刻把它们作为**独立的新回合**继续
   处理，并在那里广播它们的 `user` 事件（与 `drainSteering` 同一时机语义），不会滞留到用户下次输入。

> **中断**：每个回合有自己的可取消 context（`Agent.Interrupt()` 触发，CLI 的 Ctrl+C /
> `/stop`、网页的 Stop 按钮 / `/stop`）。中断取消正在进行的模型调用或工具调用，语义按
> **有没有工具调用已经开始**分两类：
>
> * **没有任何工具调用开始**（正在等模型；或回复已到达/仍在流式，但第一个调用尚未开始）：
>   * 服务商已经流出的**正文**保留为一条 assistant 消息（连同一起流出的思考）：流式过程中它由
>     `assistant` 事件定稿（前端把正在绘制的行收尾，回放缓冲记录这一行），完整到达的回复则本来
>     就已入历史。消息里携带的 tool 调用**全部丢弃**——它们一个都没开始执行，而被截断的回复本身
>     可能只含半条调用——丢弃数量以 `info`（消息已落库时写在 `interrupted` 文案里）说明；这种
>     情况下调用是**从已记录的消息上摘掉**的，因此不会为任何调用记 tool 回答。随后发布
>     `interrupted` 并结束回合。
>   * **正文 trim 后为空**时看 `agent.include_only_think`：开启（默认）且回复**流出了思考**时，思考作
>     为一条 assistant 消息（`reasoning_content`，无正文）**保留**下来，其中携带的 tool 调用照旧全部
>     丢弃；关闭、或思考也为空时整条 assistant 消息**不保留**——空 content 的消息对下一次请求毫无
>     意义。用户消息仍留在历史里。
>   * 连一个字符都没流出（严格处于「等模型」）：assistant 侧同样什么都不记，只发布
>     `interrupted while waiting for the model; nothing had been produced`。
>   * **用户消息在任何中断下都保留**：中断不会改写用户发过的东西，历史里始终留着那条 user 消息，
>     下一次请求时它和上面的记录一起进入上下文（没有回复的 user 消息本身依然合法）。
> * **已有工具调用开始**（单个或多个一样）：普通工具调用无法从中间打断，所以**正在执行的那一个
>   照常跑完**，它的**真实结果**就是这次调用的回答；它**之后**的调用（一个都没开始，包括同一批里
>   被展开出来的 write_file 分段）逐个发布 `interrupted by user` 并记为
>   `interrupted by user before the call started`，使 assistant 的 `tool_calls` 与 `tool` 回答保持
>   一一配对。随后发布 `interrupted` 并结束回合。两个例外：
>   * `run_script`：**强制终止**整棵进程树，并以其 `commandResult` 的 `status=interrupted`
>     反馈**已经输出的终端内容**（`session_id` 为 `null`，没有进程留在后台）；恰好在中断时
>     退出的进程仍按 `completed` 报告。
>   * `manage_session` 的 `poll`：提前结束等待（等价于一次 `wait_timeout` 到期），反馈
>     `status=running` + `session_id` + 已有输出，`warning` 说明 poll 被中断、进程仍在运行——
>     poll 只是观察者，绝不去动它监视的进程；下一次 poll 继续接着读。
>
> 以上反馈都**只写进消息历史**（`assistant` / `tool` 消息）：回合就此结束，不会再带着它们请求
> 模型——它们随**下一条用户消息**一起进入上下文。中断期间产生的 `assistant` / `tool_call` /
> `tool_result` / `info` / `interrupted` 事件与普通轮次一样广播给各前端，因此两端看到的行与历史
> 一致。之后 Agent 回到空闲，等待下一条用户消息开始新回合。若某个工具既不能中断又迟迟不返回，
> 回合会一直等到它返回——这正是"普通工具调用中间无法中断"的直接后果。

> steering 的语义：忙碌时用户输入不会丢失，而是在下一个安全点（下一轮 LLM 调用前）
> 插入到对话里，因此 Agent 能在工具循环中途「听到」新的用户指令。若消息是在**最后一轮**
> 的流式过程中到达（该轮之后已无轮次可并入），本回合会**再跑一轮**把它并进去；若连这一轮
> 都来不及（消息在回合收尾时才到，例如工具轮次已用尽 `max_tool_iterations`、或回合被中断），
> 则由 `startSteeringTurn()` **立即开一个新回合**处理它。
>
> `user` 事件（也就是前端画的那条消息行）**不在入队时广播，而是在消息真正进入对话时**
> （`drainSteering()` / `startSteeringTurn()`）才广播：这样消息行落在它进入对话的准确位置——
> 它所打断的那条回复之后——前端无需任何"暂存/挪位"的猜测，刷新后的回放顺序也天然一致。
> 在事件到达之前，发送方自己显示一条 **pending**（待发送）提示：网页是带 `· pending` 标记、
> 且把本轮后续行都插在其之前的 pending 行（Agent 发送时原地转为普通行），终端是提示区里的
> 灰色 `(pending)` 行（正式行随后按事件位置打印）。

## 事件总线

`agent.Bus` 是一个广播总线：`Subscribe()` 返回带缓冲的 channel 与取消函数，
`Publish()` 非阻塞地投递给所有订阅者（订阅者过慢时丢弃，避免阻塞 Agent）。

事件类型（`EventType`）：`user`、`assistant_delta`、`reasoning_delta`、`assistant`、`tool_call`、
`tool_result`、`info`、`error`、`compacted`、`usage`、`turn_done`、`interrupted`。

`user` 事件带 `source` 字段（`cli` / `web` 等），前端据此决定标签与是否回显自己的输入；它由
`drainSteering()` / `startSteeringTurn()` 在消息进入对话时广播（见上），因此各端的行顺序与
上下文里的消息顺序一致。
每条事件都带 `time`（RFC 3339，总线在发布时补上当前时刻）：`user` 事件是消息提交的时刻，
一条回复的 `reasoning_delta` / `assistant_delta` / 最终 `assistant` 则共用同一个**回复开始时刻**
（`callLLM()` 在**第一个流式块到达**时记下，思考块也算；没有块的非流式回复留零值，总线在最终
`assistant` 上补上整条消息到达的时刻），`compacted` 事件的 `time` 就是这次压缩发生的时刻。
CLI 与 web 都用它画出消息旁的灰色时间戳（`compacted` 的摘要块也一样，见
[web.md](web.md#消息时间)）。
`usage` 事件带 `tokens` / `context_window`，用于上下文用量显示。它在上下文增长的每个时点
（回合开始、每次模型回复、每轮工具执行后）广播，因此 tool 循环进行中前端也能实时刷新；
`tokens` 以接口返回的 `usage.prompt_tokens` 为基准，再加上此后追加消息的估算。
它还在**上下文被替换或窗口变化**的非回合时点广播，让用量显示立即跟上而不必等下一次模型回复：
`Agent.Load`（`/load`、启动恢复）、`Agent.Reset`（`/clear`、`/new`）、`Agent.SwitchLLM`
（`/switchapi` 或 Web 模型下拉，窗口属于接口）以及 `doCompact`（`/compact` 或自动压缩，
压缩后以摘要重算）。
`reasoning_delta` 携带模型「思考」增量文本，CLI 与
web 各自流式渲染（CLI 为 `[thinking]` 块，web 为 thinking 行），并和可见回答一样按
`ui.markdown` 设置渲染 Markdown；定稿后的思考写入 assistant 消息（`reasoning_content`）并随后续请求回传，供 preserve thinking 模板使用。
`compacted` 携带 `summary`（压缩后的累积摘要）：CLI 与 web 都据此在**截断处**显示摘要——
CLI 打印 `[summary]` 块，网页追加一条 `summary` 行；两端都按 `ui.markdown` 渲染摘要
Markdown，因此「详细消息到此为止」的位置一致。

CLI 与 web 各订阅一次即可；web 侧再多路复用给每个 WebSocket 客户端。web 在启动时用当前
会话（含恢复的历史）播种一份内存回放缓冲，并把之后每个事件追加进去（思考增量合并成一条
`reasoning` 行；`compacted` 在信息行之后追加一条 `summary` 行），新连接的浏览器先收到这份
快照（`history_start` + 若干 `history_rows` + `history_end`，按批次分帧），因此思考、工具行、
摘要行与 info/error 标记在刷新或后开网页时都不丢失，且不受压缩影响：压缩不删回滚里的旧消息，
只在切点插入摘要行；而恢复的会话因为切点之前的消息本就不在，摘要行是回放缓冲的第一行。


## 网络与超时

`internal/llm` 不对整个请求设置总超时（流式回复可能持续数分钟），而是分两层控制：

* 传输层 `ResponseHeaderTimeout`：等待响应头超过预算即失败；
* `idleGuard`：包装响应体，每读到数据都会重置看门狗，只有在超过
  当前接口的 `providers[].timeout_seconds` 秒没有任何数据时才取消请求，并返回可操作的错误
  （提示提高该值）。

因此「慢但持续输出」的回复不会被截断，而真正停滞的连接会很快失败。该值可用
`--timeout SECONDS` 覆盖，`0` 表示关闭。

## 并发模型

* `Agent.mu` 保护 `history`、`summary`、`busy`、`usage`、`usageAt`。长操作（LLM 调用、工具执行）
  都在不持锁的情况下进行。
* 只有一个回合在运行；`busy` 标志保证同一时刻至多一个 `runLoop`。
* 命令执行引擎内部为每个后台会话维护独立的锁与信号 channel（`outputCh`/`doneCh`），
  支持长轮询 `poll` 与并发 `kill`。
* Web 的每个连接有独立的读协程，外加一条写协程（每条连接一个发送队列，见 `internal/web/client.go`）：
  注册连接与取快照在镜像的锁内完成（保证「快照 + 之后的事件」恰好一次），序列化与 socket 写入
  在锁外进行，因此慢/卡死的浏览器不会阻塞 Agent、CLI、其它页面或其它 HTTP 端点。
  流式增量的合帧同样只在镜像的锁内做「记录 + 合并」，写出由各连接的写协程负责
  （见 [web.md](web.md#省电移动端与隐藏页面)）。

## 运行时切换 LLM 接口（`/switchapi`）

`config.json` 的 `providers` 是接口数组；启动取**第一个 `enabled`** 的接口（`config.ActiveLLM`）。
运行时可切换（CLI `/switchapi <name|序号>`、Web 侧栏模型下拉），**只改内存、不写回文件**。

* `Agent.SwitchLLM(client, contextWindow, maxTokens)`：在 `Agent.mu` 下换掉客户端与由接口派生的两个数字
  （该接口的上下文窗口、`max_tokens`）。`callLLM` 读客户端时持同一把锁，因此切换只会在空闲时发生
  （`busy` 时拒绝），不会落在某次请求中间。压缩器与 agent 共用同一客户端与这两个数字。
* `apiRuntime`（`session.go`）持有接口列表与当前序号、并负责切换：重建 `llm.Client`、调用
  `SwitchLLM`、按新接口的 `media_types` **刷新多媒体附件能力**（`upload_media` 工具与 Web 附加控制），
  然后通知前端（CLI 的模型标签、Web 的设置帧）。
* 这等价于「在另一个接口上热重载」的最轻实现：除客户端与附件能力外，程序启动时构建的一切
  （工具注册表、MCP 连接、会话、服务端与总线）**都与接口无关**，因此无需重建。

## 持久化

* 状态目录：`<当前工作目录>/.lightagent/`，**启动时创建**（先放目录锁 `.lock`，见
  [目录锁](#目录锁)）；**退出时若目录里只剩锁文件，整个目录一并删除**，因此未保存会话
  （`--no-save`、一次性 `-p` 运行、退出时选择不保存）不会留下任何目录或文件。
* 会话文件放在 `.lightagent/sessions/`：每个会话有自己的文件名（`/saveas` 起的名字），
  `session.json` 只是**没有名字**的新会话的默认名；`/list` 列出的即这里的内容。旧布局把会话文件
  直接放在 `.lightagent/`，启动时（持锁后）会**自动移入 `sessions/`**，老会话不会丢。`--session PATH`
  仍用指定文件所在的目录，不建 `sessions/` 子目录。当前会话文件是**可变**的：`/saveas`、`/load`、
  启动恢复都会把它指向具体文件，之后的 `/save` 更新那个文件；`/new` 指回默认的 `session.json`，
  `/clear` 只清空对话、保留当前文件（之后的 `/save` 仍更新同一个文件）。
* 时机：运行时**只保存在内存**（不再有每回合 persist 回调）。写盘只有两条路径：
  `/save` 命令，或退出时询问「是否保存」并确认（默认是）；`/saveas` 则写到新文件。
* `/save` 与退出保存都写回**会话自己的文件名**；没有名字的新会话写 `session.json`，并先把已存在的
  `session.json` 归档为 `session-<YYYYmmdd-HHMMSS>.json`（同名冲突时追加序号）备份。备份只在
  该文件**不归本会话**时发生：由 `session.json` 恢复、或上一次保存已写到它的会话，都按「会话就在
  这个文件里」就地更新、不再备份（于是「恢复 session.json → `/save` → 退出」全程只留一个文件）。
* 启动时**载入目录里最新的会话文件**（按修改时间，不再是固定的 `session.json`），询问是否恢复
  （默认是）；恢复后当前文件即该会话，之后的保存写回同一个文件。选择「否」时旧文件原样留在磁盘
  （不再改名归档），本次从没有名字的新会话开始。`-r`/`--resume` 直接恢复且不再询问；非交互 stdin
  无法询问，按默认值处理。
* 格式：

```json
{
  "version": 1,
  "updated_at": "2026-01-02T15:04:05Z",
  "model": "gpt-4o-mini",
  "summary": "累积的上下文摘要（可空）",
  "messages": [ { "role": "user", "content": "..." } ]
}
```

* **带附件的消息存路径，不存内容**：媒体 part（图片 / 音频 / 文件）平时是内联载荷（data URI、base64），
  写盘前由 `llm.ReferenceMedia` 换成「读自哪个文件 + 媒体类型」（`path` / `mime`，载荷留空），
  会话文件因此不会因为一张图片膨胀成几 MB；恢复会话时 `llm.ResolveMedia` 按路径把内容读回来，
  文件已被删除时该 part 保持「无载荷」，请求侧（`requestMessages`）用一句不可用文本 part 代替它。
  `path`/`mime` 只存在于磁盘（与该 part 的内存结构）上，永不进入发给服务商的请求体。

读写均为「写临时文件 + 原子重命名」。

## 目录锁

一个状态目录（默认 `<当前工作目录>/.lightagent/`）同时只允许一个实例：

* `internal/lock` 在 `<状态目录>/.lock` 上加独占锁——Unix 用 `syscall.Flock(LOCK_EX|LOCK_NB)`，
  Windows 用 `LockFileEx`（`LOCKFILE_EXCLUSIVE_LOCK|LOCKFILE_FAIL_IMMEDIATELY`，经 `kernel32.dll`
  的 `LazyProc` 调用），不用任何第三方库。
* 第二个实例在同一个目录启动时立刻失败（`another lightagent instance is already running in <目录>`，
  退出码 1），而不是等待或共用会话文件；许可目录不同（默认情形）仍可并行运行。
* 没有原生锁原语的宿主（AIX、Plan 9、js/wasm 等）退回 `O_CREATE|O_EXCL` 独占创建。
* 释放：正常退出与信号退出（`shutdown.go` 的钩子）都会解锁、删 `.lock`，并在目录里再没别的
  文件/目录时把目录一起删掉；Web 重启交接也会释放，好让用 `--resume` 起的替换进程接管。
* 锁是 advisory 的且绑定在打开的文件上：进程被强杀时操作系统会自动解锁，遗留的 `.lock`
  文件不影响下次启动（下一次直接复用并重新加锁）。

## 上下文压缩（append_instruction 单一模式）

摘要的生成方式复用同一系统提示与消息布局，末尾追加压缩指令。保留多少最新消息由
`context.summarize_keep` 配置（`budget_percent` + `turns`，自动与手动各一份），切分按
**Turn 边界 + token 预算 + 回合数上限**三条规则进行；**默认两项都是 0，即不保留任何原始消息**。

### 触发

**用量** ≥ `context_window * summarize_token_percent%` 即压缩。用量 = 接口返回的
`usage.prompt_tokens`（上一次请求的真实大小）**加上**此后追加消息的**下限估算**；没有上报值时
（进程刚启动、会话刚恢复、历史被回退到锚点之前）退回对整份请求的估算（系统提示与摘要都算在内）。
两把尺不相比：整份历史的估算只是下限计数，**不与上报值取大**，否则压缩会在设置的百分比之外提前
触发（曾出现设置 80%、实际 60% 就压缩：报告值 157125 而字符估算已到 209715）。
**服务商直接拒绝（上下文超限）时不受该阈值约束**——估算对媒体 part 与超大 tool 反馈
只是粗略计数，被拒后由引擎主动做一次溢出恢复（见
[溢出恢复](#溢出恢复provider-拒绝后回退--摘要--重发)）。

### 保留（`summarizeTailCut`）

**默认不保留任何原始消息**：整段被压缩历史都换成累积摘要。`context.summarize_keep` 只是让人可以
放宽这一点（`budget_percent` 与 `turns`，自动压缩与手动 `/compact` 各一份），算法与切分规则不变：

1. **Turn 边界**：Turn = 一条 `user` 消息及其之后的全部 assistant/tool 消息，直到下一条
   `user`。保留窗口**总是从某条 user 消息开始**，因此 assistant `tool_calls` 与 `tool`
   结果永远不会被切开，窗口也不会悬挂在孤立的 tool 结果上。
2. **token 预算**：`available = context_window - max_tokens`（两者都取自当前接口，≤0 时回退
   `context_window`）；预算 = `available * budget_percent / 100`，**默认 0，即不保留任何原始消息**。
   预算按 Turn 的**下限估算**（见 [token 估算](#token-估算estimatetokens)）累加，因此保留窗口的真实
   token 可能略高于预算。
3. **回合数上限**：最多保留 `turns` 条 user 消息（完整 Turn），**默认 0**。
4. **自新到旧累加**：从最新的 Turn 开始向前保留，只要加入下一个更旧的 Turn 后仍
   **严格小于**预算且未超过回合数上限；两者谁先触顶谁停止（默认预算为 0，因此一个都留不下）。
5. **无「至少保留最新一轮」回退**：当连最新一个 Turn 都达到/超过预算时不保留任何原始
   消息，整个尾部都进入摘要（`safeCut == len(history)`）—— 这正是默认配置下的常态。
6. 当整个尾部本就落在预算内时返回「无事可做」，不触发压缩（默认预算为 0，因此只可能出现在
   低于两行的历史上，被调用方按「无需压缩」跳过）。
7. **user 消息保底**：若压缩后一条消息都不剩（连触发本轮的那个 user 消息也被压掉，见第 5
   点），引擎会先补一条 `[engine] Context summarized, continue.` 的 user 消息再发请求 ——
   聊天模板要求请求里至少有一条 user 查询，否则接口直接返回 400。

**为什么默认一条都不留**：有些推理引擎在**回退**下不保存 prompt 缓存 —— 一旦保留最新几轮原始消息，
压缩后的实时请求前缀就回到更早的位置（比上一次请求更短），缓存随即失效；一条都不保留时，压缩后的请求
是「系统提示词 + 摘要」开头的实时前缀，缓存仍然有效，而被放弃的最新几轮对话本身已经写进摘要。
溢出恢复（`summarizeModeOverflow`）**固定不保留、没有配置项**：服务商刚证明估算偏小，只有摘要能保证
重发的请求装得下。

被压缩的部分（切点之前）按 append_instruction 模式交给模型总结：总结请求原样复用**实时请求的
请求头** —— 系统提示（基础提示词 + unlock 规则 + MCP 全局信息 + 运行时行 + 工作目录行）与同一份
tools 声明都由 `Agent.livePrefixLocked()` 这一处渲染（实时请求 `callLLM` 也从同一个快照取消息列表与
tools，两侧因此不可能来自不同状态），摘要由 `headMessages()` 放到配置指定的位置，
只是把它放到被压缩消息之前、并在末尾追加压缩指令：请求因此是实时请求的**单侧延长**——被压缩消息
之前逐字节相同、不重排不重渲染，新增的 token 只有末尾那条压缩指令。因此模型总结时所处的环境与产生这些消息时一致
（基础提示词以下的段落不会被丢掉），请求前缀与实时请求逐字节相同——服务端提示缓存仍可命中该前缀，
不会被每次压缩重置。压缩本身仍有代价：摘要文本变了，所以**下一次实时请求**的前缀从摘要那里就与缓存
不同，摘要之后的消息要重新 prefill 一次（默认布局下摘要就是紧跟系统提示词的第一条用户消息，代价因此
最小），而总结调用自己完全命中缓存。压缩指令（`summarizeInstruction`，按本次请求的实际情况生成）告诉模型：它写出的报告
是**下次对话唯一的历史上下文**，必须自足。摘要放进**系统提示词**（`agent.summary_in_system_prompt=true`）
且已经有摘要时，再补一句**新摘要会替换 `# CONVERSATION SUMMARY` 段里的摘要**——系统提示词是特殊情形，
需要点明；默认布局下报告本身就是那条 `[engine]` 摘要消息，无需额外说明。返回的报告即新的累积摘要，
是全量更新（内容包含旧摘要）而非增量追加；摘要失败则保留原摘要并退化为直接丢弃被压缩消息，回合继续。
CLI `/compact` 会以**手动模式**触发，保留策略取自 `summarize_keep.manual`（与主动压缩的
`summarize_keep.auto` 相互独立，默认都不保留）。
压缩完成后发布带 `summary` 的 `compacted` 事件，CLI 与 web 便在截断处显示它；恢复会话时
摘要出现在历史消息之前（网页日志区的第一行），即「此前的对话只以摘要形式保留」。
由于总结本身是一次模型调用（可能较慢），**压缩开始前**先发布一条 `info`
（`compacting context: summarizing N of M messages`），两端因此马上能看到「正在压缩」，
不会在等待期间毫无反馈；没有可压缩内容时不发任何事件，由命令调用方回复「无需压缩」。

### 摘要的放置（`agent.summary_in_system_prompt`）

该开关决定**发送时**累积摘要放在哪里，默认 `false`（第一条用户消息）：

```
system: <基础提示词 + unlock 规则 + MCP 信息 + 运行时行 + 工作目录行>
user:   [engine] CONVERSATION SUMMARY:
        <累积摘要>
user:   <历史里的第一条 user>
...
```

* `false`：摘要作为**第一条用户消息**发给模型（`[engine]` 开头，和 `[engine] Context summarized,
  continue.` 同一种标记，提示模型「这段是引擎侧信息」）。
* `true`：摘要追加到系统提示词末尾的 `# CONVERSATION SUMMARY` 段。

它只作用于发送前组装的消息列表（`headMessages` / `Agent.buildMessagesLocked`）；压缩的总结请求
按同一布局携带摘要（见 `livePrefix.head`），因此与实时请求的前缀一致，压缩指令（`summarizeInstruction`）
也只点名本次请求实际携带摘要的那一处。

### token 估算（`EstimateMessageTokens`）

**下限**计数，单位就是 tokenizer 真正切出的单位：英语单词、数字、代码片段按**空格切分**的每一段算
1 个 token，中文（以及同属不分词书写的假名）**逐字**算 1 个 token；连续文本每多 4 个字符再加 1 个
token，所以长串（压成一行的 JSON、base64 载荷）不会塌成 1 个 token。content、reasoning、
tool call 的 name/arguments/id 都计入。媒体 part
按**个数**粗算（`mediaPartTokens`，每 part 固定值），不按 base64 字节数：否则一张图片就会被算成
几十万 token。估算只用于两处：**上报值之后追加的消息**（触发与用量显示）与**保留预算**的 Turn
累加；它不与上报值比较，也没有「每消息固定开销」——两把尺相比正是压缩提前触发的原因。
正因它只是下限，估算可能小于真实占用，被服务商拒绝时走下面的溢出恢复。

### 溢出恢复（provider 拒绝后回退 + 摘要 + 重发）

估算对媒体 part 与超大 tool 反馈只是粗略计数，所以请求可能**已经超出窗口而估算仍说「放得下」**：
tool 循环里一条超长的反馈（整份文件、读回的图片）、带图片的用户消息，都是常见的触发者。这时
服务商返回的错误才是唯一可靠信号：`llm.IsContextLengthError` 按状态码 + 各家文案/错误码判定
（`context_length_exceeded`、`maximum context length`、`prompt is too long`、
`maximum number of tokens` 等，见 `llm.contextLengthMarkers`）。被判定的失败不再直接结束回合，
而是做一次**溢出恢复**（`Agent.recoverContextOverflow`），也就是「回退这条消息 → 自动摘要 →
重发被退回的消息」：

1. **回退**（`overflowRollbackCut`）：把这条被拒请求带来的最新消息从上下文里拿出去 —— 本轮产生
   的 assistant/tool 行（超长的工具反馈就在这里），以及**没有得到回答的 user 消息**（本轮自己的
   消息，以及失败调用前刚并入的 steering 消息）。更早的、已被回答过的历史**保持不动**，接着交给
   压缩；这些被回退的 user 消息随后会原样重放。
2. **摘要**：以 `summarizeModeOverflow` 跑一次压缩，**保留预算为 0**（该模式没有配置项，永远不留原始
   消息）——估算刚被证明偏小，只有
   摘要才能保证重发的请求装得下，因此整段幸存历史都被换成累积摘要（`compacted` 事件的
   `context compressed: N -> 1 messages`）。被回退的 user 消息**刻意不进入这次总结的批次**：
   它们带的附件（大图片、大文件）很可能正是超限的原因，若一起发给模型，总结调用自己也会被拒。
3. **重发**：把被回退的 user 消息重新放回摘要之上（`replayMessages`），随后 `continue` 重新调用
   模型——「同一条消息，在压缩后的上下文里再发一次」。重放**不重发 `user` 事件**：这些行在进入
   对话时就已广播，再播一次会让两端多画一行；若压缩后只剩下 `[engine]` 标记，则标记被摘掉
   （重放的消息本身就是请求需要的 user 查询；若一条 user 消息都没有，标记会补上）。

细节：

* 恢复过程在总线上自报：先一条 `info`（`context length exceeded: rolled back N message(s),
  compressing the context and retrying`），随后是常规的 `compacting context: …` 与带 `summary` 的
  `compacted`，因此两端都能解释这段额外等待；被回退的旧行仍留在终端/网页日志里（与压缩一致，
  只在切点插入摘要行）。
* **什么都没有释放时不重发**：若回退只拿掉了 user 消息、而幸存历史又无可压缩的内容（整条消息
  本身就超出窗口，例如一张过大的图片），重发等于把刚被拒的请求原样再发一遍——此时直接按服务商
  的错误结束回合，且**历史保持原样**（消息仍在对话里，用户可自行处理）。
* 恢复后紧接的一轮**跳过主动压缩**（`skipCompact`）：那次压缩只会把刚重放的消息吃掉。
* 单个回合最多恢复 `maxContextOverflowRecoveries`（2）次，之后按服务商的错误结束回合，避免在
  「模型又产出一条超长反馈」的循环里打转。
* 总结调用本身也可能被同一原因拒（要压缩的历史就是超限的那份）：按既有失败路径处理——保留原
  摘要、丢弃被压缩的消息，恢复照常继续（此时上下文反而更小）。

## 系统提示词优先级

`agent.md`（程序目录） > `config.agent.system_prompt` > 内置默认（`agent.DefaultSystemPrompt()`）。
`agent.md` 支持 `@include("路径")` 片段导入（文件/目录、相对/绝对、可嵌套），在读取时展开。
详见 [configuration.md](configuration.md)。

基础提示词之后按顺序追加能力段落：① 存在锁定函数时的全局 unlock 规则；② 每个已连接 MCP server 的
MCP 全局信息；③ 运行时环境行（`agent.RuntimeInfo()`，每次运行生成，**不写进** `agent.md`）；
④ 工作目录行（`agent.include_working_dir`，默认开启，`agent.WorkingDirectoryInfo()` 在 `agent.New`
时生成一次，**只有路径**，不列出目录内容）；⑤ 累积的上下文摘要（**仅** `agent.summary_in_system_prompt = true`；
默认摘要不在系统提示里，而是作为一条独立的 `[engine]` 消息紧跟其后，见
[上下文压缩](#摘要的放置agentsummary_in_system_prompt)）。

## MCP 工具发现 / unlock 控制面

MCP unlock 发现机制，由注册表 + 三个控制面工具组成：

* **注册表**（`internal/tools/registry.go`）区分 *core* 与 *deferred* 工具。deferred 工具（锁定函数）不会被 `Definitions()` 下发到模型，只有在其名称有有效 grant 时 `Get`/`ExecuteArgs` 才允许执行，否则返回 `tool is locked` 错误。
* **`tool_search_tool_bm25`**（`search_tool.go` + `bm25.go`）在 deferred 库上排序：先按关键词匹配率降序（低于 `min_match_rate` 的直接丢弃），匹配率相同再按 BM25 分数降序，只回名称+描述；引擎按注册表 `Version()` 缓存，仅在库变化时重建。发现**不注册、不授权**。
* **`unlock_tool`**（`unlock_tool.go`）写入 TTL grant 并下发规范化 XML `<tools>` schema；`ctx` 携带的 `UnlockLookup` 检查该 schema 是否仍在**当前有效（未压缩）上下文**里——在就只刷新授权并提示“之前已发过”，被压缩/淘汰则重新下发。
* **`dynamic_call`**（`dynamic_call.go`）按名转发 `ExecuteArgs`，复用注册表的授权闸门。
* **TTL**：`Agent.runLoop` 在**每个工具执行轮结束**调用 `reg.TickTTL()` 递减授权（与原 unlock 模式一致）；授权过期不影响函数仍可被搜索发现，模型重新 `unlock_tool` 即可。
* **系统提示词**：只在**存在锁定函数**时注入两节——① 一条**全局 unlock 规则**（`agent.ToolUnlockRule()`，整个提示词里只出现 1 次）；② 每个连上的 MCP server 一条 **MCP 全局信息**（配置键名 + 工具数 + `registered as locked tools; … unlock_tool … dynamic_call`，再附上 server 在 `initialize` 里返回的 `serverInfo.name/title/version` 与 `instructions`）。两节都不列出任何函数名，故前后缀字节稳定（KV 前缀友好）。

### MCP 客户端

`internal/mcp` 是纯标准库实现的 MCP 客户端：启动时按 `tools.mcp.servers` 连接每个启用的 server（`internal/mcp/manager.go`），完成 `initialize` 握手并调用 `tools/list`；每个 server 工具被包装为 `tools.Tool`（名字 `mcp_<server>_<tool>`）并以 `RegisterDeferred` 注入**锁定函数库**——它永不进入模型的 `tools` 声明，搜索只回报名称，只有 `unlock_tool` 才把 schema 带进对话。工具调用 `tools/call` 的结果经 `renderCallResult` 拍平为文本返回。

* 传输：`stdio`（子进程，按行分隔 JSON-RPC）、`http`/`streamable-http`（每次 POST，响应为 JSON 或 SSE；回传 `Mcp-Session-Id`）、`sse`（旧版长连接事件流 + endpoint POST）。
* 单个 server 连接失败不影响其他 server，错误在启动时打印。
* 关闭：`Manager.Close` 并发关闭每个连接。stdio server 先收到 **stdin EOF**（协议级关闭，server 借此自行退出并清理），2s 宽限后仍未退出才 **停止这个进程本身**——MCP 只负责自己启动的那个进程，它派生出来的进程不属于我们（见 [进程树与退出](#进程树与退出)）；`http` 断开空闲连接、`sse` 取消长连接。

详见 [tools.md](tools.md#mcp-客户端)。

## 进程树与退出

`internal/proc` 统一了子进程的启动与停止，并区分两种归属模式：

* **树模式 `Start`**（`run_script` 会话）：孩子成为自己进程树的根（Windows 加入 kill-on-close 的 Job Object；Unix 新建进程组），`Kill` 结束整棵树——只看直接子进程是不够的（shell、`cmd.exe`、`npx` 之后还有真正在跑的程序）。Windows 直接 `TerminateJobObject`；Job 分配失败（如本进程处在一个禁止嵌套的 Job 里）时退回 `taskkill /T /F`。`Reap` 在根进程被 `Wait` 之后调用：根退出时留下的后台子进程同样被清理。
* **单进程模式 `StartProcess`**（stdio MCP server）：只跟踪并停止**自己启动的那个进程**，它派生的进程一律不动。关闭顺序是「先礼后兵」：关 stdin（MCP 协议的退出信号）→ 最多等 2s → 仍未退出就强制结束该进程，不再多等。
* **退出不会卡住**：所有等待都有上限——Unix 的树终止是 `SIGTERM` → 300ms 宽限 → `SIGKILL`，`taskkill` 带回退有 5s 超时，MCP 关闭只等该进程 2s（`cmd.WaitDelay` 同值：即使 server 把 stdout/stderr 留给别的进程持有，也不会一直等下去）。即使进程「杀不掉」，本程序照常退出。
* **`Shutdown`**：主进程退出时调用（`ExecEngine.Close` 亦会结束所有运行中的会话），按各自模式停止所有仍存活的进程。树模式在 Windows 上的 kill-on-close 意味着即使 lightagent 被强杀（`taskkill`、崩溃），Job 句柄随进程关闭，整棵树仍会被 OS 带走；单进程模式没有这层保护（MCP server 只有在 lightagent 走正常退出/信号路径时才被停止）。Unix 无法拦截 `SIGKILL`，但 `SIGINT`/`SIGTERM`/`SIGHUP` 由信号看门狗处理（`shutdown.go`：先跑注册的清理钩子——例如把终端从 raw 模式恢复——再停止所有子进程，退出码 `128+signal`）。

> 交互式编辑器本身不依赖信号：raw 模式下 Ctrl+C 是一次按键（中断当前回合），控制台不会产生 `SIGINT`；信号看门狗覆盖的是非交互运行、`kill`/SIGTERM 以及终端挂断（Unix 的 SIGHUP）。


