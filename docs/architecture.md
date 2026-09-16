# 架构

lightagent 是一个单进程、多协程的微型 Agent。除 `golang.org/x/text`（字符集转换）外
只依赖 Go 标准库。

## 模块与职责

| 包 | 职责 |
|----|------|
| `internal/config` | 读取程序目录的 `config.json` 与可选 `agent.md`，提供默认值与校验 |
| `internal/llm` | OpenAI 兼容的 `/chat/completions` 客户端：流式 SSE 与非流式解析（含 `reasoning_content` / `reasoning` 思考透出）、工具调用聚合、`extra_body` 合并、空闲超时看门狗 |
| `internal/agent` | 回合循环（tool loop）、steering、事件总线、上下文压缩、系统提示词 |
| `internal/tools` | 工具接口与注册表（含 MCP unlock 的 deferred/grant 控制面、BM25 发现搜索、`unlock_tool`、`dynamic_call`）、命令执行引擎与会话池、文件工具、字符集编解码 |
| `internal/mcp` | MCP 客户端（纯标准库）：JSON-RPC 2.0 over stdio / Streamable HTTP / HTTP+SSE；配置驱动的 `Manager` 与 `tools.Tool` 适配器 |
| `internal/store` | 单会话持久化（`CWD/.lightagent/session.json`） |
| `internal/cli` | 彩色 REPL、斜杠命令、Markdown 流式渲染（未完成行作为预览绘制在提示符上方，按终端宽度折行、最多 8 行，因此超出首行的文本也边收边显示）、提示区原地逐行重绘（不整块擦除，老式 Windows 控制台才不会闪屏）、`[thinking]` 思考流式块（同样应用 Markdown）、异步渲染事件 |
| `internal/web` | HTTP + WebSocket 实时镜像；stdlib 实现 RFC6455；内嵌 marked + DOMPurify 供浏览器渲染 Markdown |
| `internal/markdown` | 无依赖的 Markdown → ANSI 渲染（CLI 用），按行流式输出并暴露未完成行（`Pending`）供预览 |
| `internal/termcolor` | ANSI 彩色封装（检测到终端支持才着色） |

## Agent 回合循环

入口是 `Agent.Submit(text)`：

1. `Submit` 用互斥锁判断当前是否忙碌。
   * 空闲：置 `busy=true`，发布 `user` 事件，启动 `runLoop` 协程。
   * 忙碌：把消息推入 `steerCh`（steering），并发布 `user` 事件。
2. `runLoop` 执行：
   1. 追加第一条 user 消息并广播一次 `usage`（前端立刻反映新消息带来的用量）。
   2. 每轮开始时 `drainSteering()`：把 `steerCh` 中的消息按序追加为 user 消息。
   3. `compactIfNeeded()`：估算 token 超阈值则压缩（见下）。
   4. `callLLM()`：构造 `[system, ...history]`，附带工具定义，流式调用；模型可见回答的增量
      文本发布为 `assistant_delta`；若服务商返回思考内容（`reasoning_content` / `reasoning`），
      其增量先发布为 `reasoning_delta`。
   5. 追加 assistant 消息（含组装后的思考 `reasoning_content`，会随后续请求回传）并广播
      `usage`。
   6. 无 `tool_calls` 时：`finish_reason=length`（被 `max_tokens` 截断）→ 自动续跑：不追加用户消息，直接保留该 assistant 消息进入下一轮（连续 3 次则停止）；否则回合结束。
   7. 逐个执行工具，发布 `tool_call` / `tool_result`，把结果作为 `tool` 消息追加；本轮结束后
      广播 `usage`（工具结果同样占用上下文），回到 2。
   8. 达到 `max_tool_iterations` 时发布 `info` 并结束。
3. 结束时置 `busy=false`、保存会话、发布 `turn_done`。

> **中断**：每个回合有自己的可取消 context（`Agent.Interrupt()` 触发，CLI 的 Ctrl+C /
> `/stop`、网页的 Stop 按钮 / `/stop`）。中断会取消正在进行的模型调用或工具调用：
> 处在「等模型」阶段（本条记录尚无任何产出）时**回滚该条用户记录**并发布 `interrupted`；
> 处在工具阶段时保留已有记录，把被中断（及其后未执行）的工具调用记为
> `interrupted by user`，发布 `interrupted` 后结束回合。之后 Agent 回到空闲，等待下一条
> 用户消息开始新回合。`exec_command` 的同步等待可被取消：中断会直接结束其进程。

> steering 的语义：忙碌时用户输入不会丢失，而是在下一个安全点（下一轮 LLM 调用前）
> 插入到对话里，因此 Agent 能在工具循环中途「听到」新的用户指令。

## 事件总线

`agent.Bus` 是一个广播总线：`Subscribe()` 返回带缓冲的 channel 与取消函数，
`Publish()` 非阻塞地投递给所有订阅者（订阅者过慢时丢弃，避免阻塞 Agent）。

事件类型（`EventType`）：`user`、`assistant_delta`、`reasoning_delta`、`assistant`、`tool_call`、
`tool_result`、`info`、`error`、`compacted`、`usage`、`turn_done`、`interrupted`。

`user` 事件带 `source` 字段（`cli` / `web` 等），前端据此避免回显自己的输入；
`usage` 事件带 `tokens` / `context_window`，用于上下文用量显示。它在上下文增长的每个时点
（回合开始、每次模型回复、每轮工具执行后）广播，因此 tool 循环进行中前端也能实时刷新；
`tokens` 以接口返回的 `usage.prompt_tokens` 为基准，再加上此后追加消息的估算。
`reasoning_delta` 携带模型「思考」增量文本，CLI 与
web 各自流式渲染（CLI 为 `[thinking]` 块，web 为 thinking 行），并和可见回答一样按
`ui.markdown` 设置渲染 Markdown；定稿后的思考写入 assistant 消息（`reasoning_content`）并随后续请求回传，供 preserve thinking 模板使用。

CLI 与 web 各订阅一次即可；web 侧再多路复用给每个 WebSocket 客户端。web 在启动时用当前
会话（含恢复的历史）播种一份内存回放缓冲，并把之后每个事件追加进去（思考增量合并成一条
`reasoning` 行），新连接的浏览器先收到含全部行的 `history` 快照，因此思考、工具行与
info/error 标记在刷新或后开网页时都不丢失，且不受压缩影响。

## 网络与超时

`internal/llm` 不对整个请求设置总超时（流式回复可能持续数分钟），而是分两层控制：

* 传输层 `ResponseHeaderTimeout`：等待响应头超过预算即失败；
* `idleGuard`：包装响应体，每读到数据都会重置看门狗，只有在超过
  `openai.timeout_seconds` 秒没有任何数据时才取消请求，并返回可操作的错误
  （提示提高该值）。

因此「慢但持续输出」的回复不会被截断，而真正停滞的连接会很快失败。该值可用
`--timeout SECONDS` 覆盖，`0` 表示关闭。

## 并发模型

* `Agent.mu` 保护 `history`、`summary`、`busy`、`usage`、`usageAt`。长操作（LLM 调用、工具执行）
  都在不持锁的情况下进行。
* 只有一个回合在运行；`busy` 标志保证同一时刻至多一个 `runLoop`。
* 命令执行引擎内部为每个后台会话维护独立的锁与信号 channel（`outputCh`/`doneCh`），
  支持长轮询 `poll` 与并发 `kill`。
* Web 的每个连接有独立的读协程；写操作由 `wsConn.wmu` 串行化并有写超时。

## 持久化

* 状态目录：`<当前工作目录>/.lightagent/`，**首次写盘时惰性创建**：未保存会话
  （`--no-save`、一次性 `-p` 运行、退出时选择不保存）不会留下任何目录或文件。
* 会话文件：`session.json`（该目录**当前**会话）。
* 时机：运行时**只保存在内存**（不再有每回合 persist 回调）。写盘只有两条路径：
  `/save` 命令，或退出时询问「是否保存」并确认（默认是）。
* 启动时若 `session.json` 存在，会询问是否恢复（默认是）；选择「否」时把它重命名为
  `session-<YYYYmmdd-HHMMSS>.json` 归档（同名冲突时追加序号），再开始新会话。
  `-r`/`--resume` 直接恢复且不再询问；非交互 stdin 无法询问，按默认值处理。
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

读写均为「写临时文件 + 原子重命名」。

## 上下文压缩（append_instruction 单一模式）

摘要的生成方式复用同一系统提示与消息布局，末尾追加压缩指令，
**保留机制也与主项目对齐**：不再是「固定保留最近 N 条」，而是 **Turn 边界 + token 预算 +
回合数上限**。

### 触发

估算 token（或接口返回的 `usage.prompt_tokens`）≥ `context_window * summarize_token_percent%`
即压缩。

### 保留（`summarizeTailCut`）

1. **Turn 边界**：Turn = 一条 `user` 消息及其之后的全部 assistant/tool 消息，直到下一条
   `user`。保留窗口**总是从某条 user 消息开始**，因此 assistant `tool_calls` 与 `tool`
   结果永远不会被切开，窗口也不会悬挂在孤立的 tool 结果上。
2. **token 预算**：`available = context_window - openai.max_tokens`（≤0 时回退
   `context_window`）；预算 = `available / 10`（自动）或 `available / 20`（手动 `/compact`）。
3. **回合数上限**：自动最多保留 3 条 user 消息（3 个 Turn），手动最多 2 条。
4. **自新到旧累加**：从最新的 Turn 开始向前保留，只要加入下一个更旧的 Turn 后仍
   **严格小于**预算且未超过回合数上限；两者谁先触顶谁停止。
5. **无「至少保留最新一轮」回退**：当连最新一个 Turn 都达到/超过预算时不保留任何原始
   消息，整个尾部都进入摘要（`safeCut == len(history)`）。
6. 当整个尾部本就落在预算内时返回「无事可做」，不触发压缩。

被压缩的部分（切点之前）按 append_instruction 模式交给模型总结，摘要与旧摘要合并
（`旧 + "\n\n" + 新`）后写入系统提示的 `# CONVERSATION SUMMARY` 段；摘要失败则退化为
直接丢弃被压缩消息，回合继续。CLI `/compact` 会以**手动模式**触发（更保守的保留窗口）。

### token 估算（`EstimateMessageTokens`）

与主项目 tokenizer 一致：`(字符数 + 12) * 2/5`，其中字符数包含 content、tool call 的
name/arguments/id 等，即约 2.5 字符/token，另加每消息 12 字符的固定开销。触发判断时
优先采用接口返回的 `usage.prompt_tokens`（若更大）。

## 系统提示词优先级

`agent.md`（程序目录） > `config.agent.system_prompt` > 内置默认（`agent.DefaultSystemPrompt()`）。
`agent.md` 支持 `@include("路径")` 片段导入（文件/目录、相对/绝对、可嵌套），在读取时展开。
详见 [configuration.md](configuration.md)。

基础提示词之后按顺序追加能力段落：① 运行时环境行（`agent.RuntimeInfo()`，每次运行生成，**不写进**
`agent.md`）；② 当前目录清单（`agent.include_working_dir`，默认开启，`agent.DirectoryListing()`
在 `agent.New` 时生成一次）；③ 存在锁定函数时的全局 unlock 规则；④ 每个已连接 MCP server 的 MCP 全局信息。

## MCP 工具发现 / unlock 控制面

MCP unlock 发现机制，由注册表 + 三个控制面工具组成：

* **注册表**（`internal/tools/registry.go`）区分 *core* 与 *deferred* 工具。deferred 工具（锁定函数）不会被 `Definitions()` 下发到模型，只有在其名称有有效 grant 时 `Get`/`ExecuteArgs` 才允许执行，否则返回 `tool is locked` 错误。
* **`tool_search_tool_bm25`**（`search_tool.go` + `bm25.go`）在 deferred 库上做 BM25 排序，只回名称+描述；引擎按注册表 `Version()` 缓存，仅在库变化时重建。发现**不注册、不授权**。
* **`unlock_tool`**（`unlock_tool.go`）写入 TTL grant 并下发规范化 XML `<tools>` schema；`ctx` 携带的 `UnlockLookup` 检查该 schema 是否仍在**当前有效（未压缩）上下文**里——在就只刷新授权并提示“之前已发过”，被压缩/淘汰则重新下发。
* **`dynamic_call`**（`dynamic_call.go`）按名转发 `ExecuteArgs`，复用注册表的授权闸门。
* **TTL**：`Agent.runLoop` 在**每个工具执行轮结束**调用 `reg.TickTTL()` 递减授权（与原 unlock 模式一致）；授权过期不影响函数仍可被搜索发现，模型重新 `unlock_tool` 即可。
* **系统提示词**：只在**存在锁定函数**时注入两节——① 一条**全局 unlock 规则**（`agent.ToolUnlockRule()`，整个提示词里只出现 1 次）；② 每个连上的 MCP server 一条 **MCP 全局信息**（配置键名 + 工具数 + `registered as locked tools; … unlock_tool … dynamic_call`，再附上 server 在 `initialize` 里返回的 `serverInfo.name/title/version` 与 `instructions`）。两节都不列出任何函数名，故前后缀字节稳定（KV 前缀友好）。

### MCP 客户端

`internal/mcp` 是纯标准库实现的 MCP 客户端：启动时按 `tools.mcp.servers` 连接每个启用的 server（`internal/mcp/manager.go`），完成 `initialize` 握手并调用 `tools/list`；每个 server 工具被包装为 `tools.Tool`（名字 `mcp_<server>_<tool>`）并以 `RegisterDeferred` 注入**锁定函数库**——它永不进入模型的 `tools` 声明，搜索只回报名称，只有 `unlock_tool` 才把 schema 带进对话。工具调用 `tools/call` 的结果经 `renderCallResult` 拍平为文本返回。

* 传输：`stdio`（子进程，按行分隔 JSON-RPC）、`http`/`streamable-http`（每次 POST，响应为 JSON 或 SSE；回传 `Mcp-Session-Id`）、`sse`（旧版长连接事件流 + endpoint POST）。
* 单个 server 连接失败不影响其他 server，错误在启动时打印。

详见 [tools.md](tools.md#mcp-客户端)。


