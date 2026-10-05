# 开发指南

## 环境

* Go 1.23+（开发环境为 1.25）。
* 唯一外部依赖：`golang.org/x/text`（字符集转换）。其余全部使用标准库。

## 构建与运行

```bash
cd lightagent
go build -o lightagent.exe .      # Windows
go build -o lightagent .          # macOS / Linux
./lightagent.exe
```

开发时用 `go run .` 会让 `os.Executable()` 指向临时构建目录，因此配置文件也会落到那里。
用环境变量指向项目内的配置即可：

```bash
# Windows PowerShell
$env:LIGHTAGENT_CONFIG = "$PWD\config.json"; go run .
# bash
LIGHTAGENT_CONFIG=./config.json go run .
```

## 常用命令

```bash
go build ./...            # 编译全部包
go vet ./...              # 静态检查
gofmt -w .                # 格式化
go test ./... -count=1    # 全部测试（会真的启动浏览器的用例默认跳过，见「测试」）
go test ./internal/tools -run Exec -v   # 单个包/用例
LIGHTAGENT_TEST_BROWSER=1 go test ./... -count=1   # 连会真的启动浏览器的用例一起跑
```

`lightagent gen-agent-prompt [-f]`：把内置系统提示词导出为程序目录的 `agent.md`。

## 目录结构

```
main.go                     入口：参数解析、help/version、子命令分发、gen-agent-prompt
session.go                  会话装配：交互 / 一次性运行、sessions 子命令、--print-config
completion.go               shell 补全脚本（bash / zsh / powershell）
internal/termcolor/         ANSI 彩色
internal/config/            config.json + agent.md
internal/llm/               OpenAI 兼容客户端
internal/agent/             回合循环、事件、压缩
internal/tools/             工具与命令执行引擎
internal/store/             多会话持久化（会话文件在 .lightagent/sessions/，含旧布局迁移）
internal/lock/              目录锁（flock / LockFileEx，纯 syscall，一个目录一个实例）
internal/textwidth/         终端列宽测量（东亚宽字符算 2 列；CLI 折行与 /list 列对齐共用）
internal/markdown/          Markdown → ANSI 渲染（CLI）
internal/passwd/            加盐摘要：sha256(盐+密码)，Go 与页面共用同一字节约定
internal/slash/             斜杠命令表：CLI 的 /help、网页的 /help 与左侧命令栏共用（含命令解析、引号参数、/list 文案）
internal/cli/               彩色 REPL + 多行编辑（Enter 换行、Ctrl+J 发送）
  escape.go                 转义序列解码（CSI/SS3、kitty CSI-u、modifyOtherKeys）
  term_windows.go           Windows 控制台 raw 输入（ReadConsoleInputW + Win32 input mode）
  term_linux.go             Linux termios raw 输入 + kitty 键盘协议请求
internal/web/               WebSocket 镜像 + config 编辑（/api/config）+ 登录（/api/login）
  index.html                页面骨架（服务端注入 markdown/结果开关与命令栏；含登录对话框）
  app.css                   页面样式（含配置表单控件与登录面板样式）
  app.js                    页面行为（对话镜像，连接受会话状态门控）
  math.js                   公式渲染（LaTeX 子集 → MathML，页面与测试共用，见 docs/web.md）
  auth.js                   登录对话框（加盐摘要、记住我、连接门控）
  config.js                 配置编辑器（表单 / JSON 双模式 + 密码控件）
  config.go                 config 读取/写回接口（写回后重启生效）
  auth.go                   会话、登录限流、同源校验、密码接口（立即生效）
  assets/                   内嵌的浏览器库（marked / DOMPurify）
docs/                       本文档
```

## 测试

| 包 | 覆盖要点 |
|----|----------|
| `main` | 参数解析（短/长/未知/冲突）、`--print-config`、`-p --json` 一次性运行、help/version |
| `config` | 默认值、往返、部分配置补默认、`agent.md` 覆盖、`@include` 展开（文件/目录/嵌套/相对与绝对路径/循环与深度/整行匹配/CRLF）、校验（含 `tools.webfetch.mode` 的取值）、压缩保留策略（`context.summarize_keep` 默认全 0、只写一半时的补默认、越界 `budget_percent`/负 `turns` 回退）、`exec` 的 `max_lines`/`max_lines_max`（默认 50/100、部分配置补默认、上界低于默认值时下调默认）、`webfetch` 配置（默认 `mode=auto`、抓取方式省略/大小写归一、`max_bytes` 负值回退、`max_lines` 0 补默认而负值保留、`attach_address` 取值与内置端点回落、空值不写入文件）、`-c` 指定路径、编辑器解析（`Parse`/`ParseStrict`：默认值补全、未知字段宽容 vs 拒绝、空/多份文档）、`MaskSecrets` |
| `llm` | `extra_body` 合并与覆盖、SSE 流式聚合、非流式解析 |
| `tools` | 行读取分页/EOF、编码侦测（UTF-8/UTF-16/UTF-32 的 BOM；无 BOM 时 NUL 奇偶 + 打分兜底；显式标签与别名；先解码再切行、按 rune 边界截断；BOM 剥离与 `bom:` 表头；二进制拒绝）、写文件三种模式与行截断（截断位置预览）、写编码的 BOM 规则（`utf-8-sig`/泛化 `utf-16`/`utf-32` 写 BOM，`utf8`/显式端序不写，`mode='a'` 不写）、编辑三种模式（`replace`/`insert`/`regex`）、GBK 往返、CRLF 保持、脚本语言选择（宿主引擎 / Python 直接引擎、`language` 的 `enum` 与默认值、工具描述里的可选值说明与 Python 版本注入、未知名报错）、子进程 stdio 的 UTF-8 模式（前置头 + `PYTHONIOENCODING` + 直通）与回退的主机代码页转换、命令快路径与后台会话（含**会话池的僵尸规则**：进程退出后保留最后输出与 `exit_code` 直到被 `poll`/`kill` 取走、取走即释放、24 小时（`zombieTTL`）没取走才清理——按**退出时刻**而不是启动时刻判定，所以一段长命令刚退出后的 poll 不再拿到 not found、`list` 显示 `status=zombie` 与 `exit_code` 并附提示行、`kill` 运行中 → `completed` + `exit_code = -1` + 最后输出 / 已退出 → `failed` + **实际退出码** + 最后输出且两者都释放会话、`input` 对已退出的会话提示 poll 取走）、会话输出缓冲的终端行模型（CR/退格只移光标不擦除、短重写留尾、CSI K/G/C/D 与跨块转义、颜色不外泄、被就地改写的行按窗口折叠、每次 poll/同步等待交接把缓冲区整份交出并清空（含正在重画的那一行；`exec_command` = start + poll，输出语义一致）、`max_lines`/`max_chars` 按每次调用施加（不跨调用、不按进程生命周期累计）、`max_lines` 的默认值与最大值（schema 的 `default`、超出上界自动截断、描述里的默认/上限与「重定向到文件」建议、`manage_session` 与 `exec_command` 同一范围）、1MB 上限按窗口打标）、`wait_timeout` = 等进程退出（`poll` 与同步等待一致，输出不提前结束等待）、`webfetch`（页面 → Markdown 正文与状态行（含 `source:` 抓取地址）、站内链接写成根相对路径、参数校验、失败反馈、**非网页/二进制内容被拒**、反馈行数上限与超长落盘（`.lightagent/webfetch/<时间>.md`，Markdown 正文，含总数与路径反馈、不限长度开关）、**超长时优先回填定位到的正文**（省略处与正文起始行写成标记，标记各自独占一行）、**定位不到正文时回填中间若干行并报告总行数**、**`method` 四种反馈方式**（默认正文、`save_as_md`/`save_as_html` 落盘并只报告路径与大小、`ignore` 只要状态行；后三者不做转换/不受 `max_lines` 影响）、**`invokejs`**（`http` 模式与不给浏览器时拒绝、脚本报告放在反馈最前面、schema 按取法提供该参数、失败与超时的时间换算）、**浏览器 profile 落在工作目录 `.lightagent/browser-profile`**、web 侧配置：默认取法/超时回落、按 `mode` 陈述取法、五种取法的接线（可见/无头/挂载）、`user_agent` 与 `max_bytes` 实际生效） |
| `agent` | token 下限估算（空格切分的每一段 + 中文字各 1 个 token、段内每多 4 个字符再加 1 个）、压缩保留策略（`summarize_keep`：默认与溢出恢复都不留原始消息、留了就必须从 Turn 边界切且不孤立 tool 消息、预算份额来自可用输入预算）、触发判定（上报值 + 追加估算，不与整份估算取大）、摘要重写（模型整篇重写，新摘要是全量更新、含旧摘要内容；指令按本次请求生成：说明报告是下次对话唯一的历史上下文，摘要在系统提示词且已有摘要时补一句替换该段摘要；摘要失败保留原摘要）、摘要放置（`agent.summary_in_system_prompt`：发送时摘要作为第一条用户消息 / 追加到系统提示词，总结请求与实时请求同布局）、steering（消息行在并入上下文的那一刻广播：末轮流式中到达则本回合续跑一轮、收尾时才到则开跟进回合，事件顺序与历史顺序一致）、消息构建（含运行时环境行：模板不含 host 信息、agent.md 提示词同样追加）、重置、上下文用量（接口 `prompt_tokens` 锚定 + 追加估算、回合中实时广播） |
| `utils` | HTML → Markdown 转换（含**站内链接写成根相对路径**、**正文定位与其在整篇 Markdown 里的行号**、默认不改行为的开关式接线）、字符集判定与解码、URL 归一化、页面抓取（HTTP 源码 / 浏览器渲染、内容类型拒绝、字节上限、**在渲染好的页面里执行脚本并等待其回调**（`_invokejs_done`，非枚举注入；值/undefined/异常/超时四条路径、报告值的截断、无浏览器时拒绝脚本））、Chromium 发现与启动/挂载/共享池、**崩溃残留清理与端口文件重读**（被杀浏览器留下的端口文件与崩溃标记不再拖垮下次启动，优雅关闭后进程真的结束） |
| `store` | 会话保存/载入往返、存在性判断、旧会话归档（时间戳 + 冲突序号）、`sessions/` 布局（根目录/会话目录分离、`--session` 仍用给定目录）、`SaveAs`/`Reset` 的当前文件切换、`Latest`（最新会话）与 `Named`（有名字 / 默认名）、`/list` 的按时间倒序与条数限制、旧布局（会话文件在 `.lightagent/` 根下）自动移入 `sessions/` |
| `cli` | 按键分类（Enter 换行 / Ctrl+J 发送 / Ctrl+Enter、Alt+Enter 在终端能上报时同样发送）、转义序列解码（CSI/SS3 整条读干净、kitty `CSI 13;5u`、modifyOtherKeys `CSI 27;5;13~`）、提示行布局（忙图标与彩色耗时在标签之后、光标回退到 `> ` 之后、空输入提示、实时上下文用量标签）、提示区原地重绘（只重画变化的行、缩行用 `EL` 清尾、行数变少才 `ESC[J`、惰性擦除：无输出的渲染事件不擦提示区）、提示区行宽（超宽输入按终端宽度折行、宽字符整字换行、窄终端不画灰色提示）、流式预览折行（未完成行按终端宽度换行、宽字符整字换行、超过 8 行只保留最新窗口）、待发送消息在提示区显示为灰色 `(pending)` 行（正式行随后按事件位置打印，无提示区时改打灰色提示）、忙图标字形回退（无盲文字形的控制台用 ASCII）、多行消息续行缩进、工具结果裁剪/缩进、banner 字段对齐、`/result` 开关、用量格式化、确认提示（恢复/保存）、消息开始时间戳（流式取第一个块——思考块也算、非流式取整条消息，灰色画在 user 行、助手回复与摘要块上（think 回复印在 `[thinking]` 头、摘要印在 `[summary]` 头）、工具行/标记行不带，当天 `15:04`、跨天带 `01-02`，一条回复只打一次，`time` 为零的旧会话/旧摘要不画）、本终端输入与其它客户端输入的消息都按事件到达位置绘制（编辑器不回显）、退出保存写回会话自己的文件名（无名字则归档备份 session.json 再写，且刚保存过不会重复留档） |
| `markdown` | ANSI 渲染、围栏代码不解析、行内强调、intraword `_`、流式分块 |
| `termcolor` | 颜色开关、`NO_COLOR`/`TERM=dumb`、非 TTY 关闭 |
| `passwd` | 加盐摘要（`sha256(盐+密码)`）与 Go/JS 一致的字节约定、盐的随机性与格式校验 |
| `web` | 会话登录（`/api/session` 的盐与状态、摘要登录与错误摘要、超时 429 限流、记住我的持久 cookie、登出、401 门控与 WebSocket 握手拒绝）、同源校验（CSRF）、密码接口（立即生效、轮换盐、登出其它会话、清除后开放且文件里留下空的 `password`、`password_salt` 随之移除）、`/api/config` 读写（打码回显、保留原密钥与密码、拒绝不可启动/未知字段的文档且不动文件、写回的文件立刻可被 `config.Parse` 读回）、配置编辑器接线（Form/JSON 双模式、每个配置项都在表单里（含 `tools.webfetch` 的 web 侧配置）、密码控件不把明文写进文档、401 触发重新登录、控件与样式）、页面公开而数据端点受保护、回放缓冲与行顺序（按事件到达顺序记录行、消息行才画在进入对话的位置、pending 行标记与"本轮输出插在 pending 行之前"、转正式后行原位更新、运行徽标里的 queued 计数——手机只留数字、词进徽标 title）、消息开始时间（`time` 字段、角色标签旁灰色 `15:04:05`、跨天加 `01-02` 日期、`agent` 右 / `you` 左镜像、user/assistant/摘要行都带、只以思考加工具调用作答（没有可见正文）的回复打在 think 行（回复结束时才落笔，有可见回答就留给回答行——实时的 `assistant_delta`、回放的 `assistant`）、一条回复只打一次、pending 行转正式时补上、随快照回放一致（reasoning 行也记 time 并按同一规则决定落哪一行）、无时间的旧会话/旧摘要不画）、贴底写入（内容一变就在同一任务里补写、判据是"位置还是页面自己写的 scrollTop"、`scroll` 事件里页面自己写下的位置不算读者动作（否则贴底写入与行变矮时的钳位会被读成上翻而熄掉跟随）、按测量两趟把内容底边对到盒子底边、视口变化（手机地址栏/键盘）时也同任务贴底，配整像素行高与 overflow-anchor:none）、回放 Markdown 的补完与不打扰读者（切回前台时一次升完整个待升级队列、补完排在拨号之后、升级前记视野第一行并按测量写回原位（跟随中不写回、一批结束补一次贴底）、队列已空的切片不写视图；重连重建整体换入：日志非空时快照的批次只收在 fragment 里、`history_end` 一次性换入并在同一任务里按测量贴底——视图不经过新对话的开头也不先看到顶部再被拽到底部，回放期间发出的消息排在最后一行之后；首屏（日志本来就空）仍按批铺开）、手机端紧凑顶栏（用量徽标只留百分比、登出为图标按钮、徽标与图标按钮间距 4px）与输入框短提示语、手机端左侧信息栏抽屉（banner 独占一行横跨整宽、内容列 minmax(0, 1fr)（顶栏撑不下时挤自己不挤正文）、信息栏只在 banner 下方且顶边贴住 banner 底边 1px、底色不透明、160ms 快速滑动、logo 点开 / 再点或遮罩或 Esc 或栏内任一动作收起、跨 1000px 断点清状态、宽屏下 logo 为纯装饰）、复制消息（角色标签旁的透明图标：`agent` 右 / `you` 左镜像对称、零尺寸 flex 项不占布局、桌面 hover / 触屏长按或轻点显现、点别处收起，Markdown 原文 / 渲染 HTML + 纯文本口味 / 屏幕上的文本，半透明模糊的图标菜单（三口味各一图标、无文字）+ 升起动画（空间不足翻转向上、reduced-motion 关闭）、刚复制的图标临时变绿且随菜单关闭清除，无 Clipboard API 的明文 HTTP 退化为选中 + `execCommand('copy')`、HTML 口味经 `copy` 事件写入，只有 user/assistant 行与摘要行带图标、菜单点击外部/Esc/滚动/重建关闭、手机 32px 点触目标且字形不压正文）、长行折叠（thinking 折到 THINKING 标签、工具调用折到工具名、工具结果折到一行——结果行的首行与其余是两个节点：折起时其余不渲染、首行按宽度截断出省略号，避免「限高裁在内边距盒上」把下一行露出几个像素；结果行的浅底框用等量负外边距抵消自身水平内边距，使 `[result]` 与 `[thinking]`/`[tool]` 的正文左对齐；点行首那条带即折/展，正文其他位置与拖选不动它；悬浮按钮中间的 +/- 开关（三枚按钮的图标排成一列：图标固定在左内边距、文字在剩余空间居中）一次折/展全部、并把该状态作为新行的出生状态，每行仍各自记住自己的状态——全折后照样能点开某一条；只存内存不落存储，user/assistant/摘要与 info/error/interrupted 不折；折/展后跟随中同任务贴底、翻上去的读者按视野第一行锚住）、公式渲染（`math.js` 经 node 跑真实用例：关系 / 取反 / 箭头 / 集合运算 / 别名（`\leg` `\greq`）等约 350 个命令、`\text` / 上下标 / 根式 / 分数 / 大算符 / `\pmod` / `\overbrace`、指数落在括号组上（`(x_i-\mu)^2`、`\left(\frac{n}{e}\right)^n`）、含符号的脚标（`\int_{-\infty}^{\infty}`）、`\binom` / `\Box`、双线与加粗字体、多行表格（裸 `\\`、`aligned` / `cases` / `pmatrix` / `array` 的 `\\` 与 `&`）、排版开关（`\displaystyle` `\limits`）不印出命令名、围栏代码与价格（含紧贴单词与纯数字）不误判、未闭合 `$` 不吞下一行、半截公式保持原文、占位符不泄漏到 HTML） |

> 含真实子进程的用例（`exec_command`）在不同平台使用不同命令（Windows 用 PowerShell，
> 其它用 `sh`），耗时约 2 秒。
>
> 「**会真的启动浏览器**」的用例（`internal/utils`、`internal/tools/webfetch_test.go` 与
> `main_test.go` 里渲染 / 挂载页面的那些）**默认跳过**，要 `LIGHTAGENT_TEST_BROWSER=1` 才运行：
> 启动的浏览器会触碰所在机器的网络栈与凭据存储，在启用了账户锁定策略的机器上，浏览器触发的失败
> 登录会把跑测试的那个账户锁掉（Windows 安全日志 4625「登录失败」→ 4740「已锁定用户帐户」，
> 调用方进程为 `chrome.exe`、登录类型 2、认证包 `Negotiate`）。设了该变量后仍会先看本机有没有
> 可用的 Chromium（没有则跳过）；`-short` 同样会跳过这些用例。
>
> `TestMathRender`（`internal/web/math_test.go`）用 `node` 跑页面自己的 `math.js`（未装 node 时
> `t.Skip`），把全部用例喂进 `protect → marked → inject` 这条真实管线，所以公式子集的改动和
> Go 代码一样被用例锁定；前端其余部分仍以页面源码断言为主（见 `pageSource`）。

### Windows 终端输入

`internal/cli/term_windows.go` 用 `ReadConsoleInputW` 读按键记录（`KEY_EVENT_RECORD`），
这样才能区分 **Enter（换行）** 与 **Ctrl+Enter（发送）**——两者都是 `VK_RETURN`，差别在
`dwControlKeyState`。同时会尝试请求终端进入 **Win32 input mode**（`CSI ?9001h`），让修饰键
穿过 ConPTY：

* 仅在 Windows 构建 ≥ 19041、stdout 为真实控制台且能开启 VT 时发出该请求；
* `LIGHTAGENT_SIMPLE_INPUT=1` 可禁用（终端不支持时的逃生开关）；
* 无法区分 Ctrl+Enter 的终端用 **Ctrl+J**（LF）发送，映射与平台无关。

`TestInputRecordLayout`、`TestClassifyKeyRecord` 与 `TestSpinnerGlyphsSupported`（`term_windows_test.go`）锁定结构体布局、
按键映射与忙图标字形回退。

### POSIX 终端输入

`internal/cli/term_linux.go` 把终端切到 raw（termios）后按字节读，SSH 走的是同一条字节通道，
所以**本地 Linux 与 SSH 会话的行为完全一致**：

* Enter 是 CR（换行）、Ctrl+J 是 LF（发送）：两者字节不同，任何终端都能区分，是全平台兜底；
* Alt+Enter 走 meta 前缀（`ESC CR`）→ 发送。`ESC` 之后是否真有后续输入用 `select` 等 50ms 判定，
  否则「按了 Esc 再按别的键」会被误当成 meta 组合（孤立 Esc 只丢弃自身，不吃下一个按键）；
* Ctrl+Enter 只有终端解歧义时才有：启动时请求 **kitty 键盘协议**（`CSI > 1 u`，退出时 `CSI < u` 撤销），
  支持该协议的终端会把和弦报成 `CSI 13;5u`（Ctrl+Enter）、`CSI 13;3u`（Alt+Enter）、
  `CSI 13;2u`（Shift+Enter，语义仍是换行）；不支持的终端忽略该请求，此时用 Ctrl+J；
* `escape.go` 负责解码：CSI 序列按「参数字节 / 中间字节 / 终止字节」整条读完（`CSI 27;5;13~` 这类
  modifyOtherKeys 形式同样识别），未知序列整体丢弃——旧实现只跳 `ESC` 加一个字节，
  `ESC [ 1 ; 5 A` 的尾巴会漏成字面文本 `;5A`；
* 解歧义后的 ctrl+字母沿用传统含义（`CSI 99;5u` = Ctrl+C = 中断、`CSI 106;5u` = Ctrl+J = 发送……），
  终端即使把控制键也改写，编辑器按键不变；
* `LIGHTAGENT_SIMPLE_INPUT=1` 与 Windows 一致：不发该请求。

`TestClassifyEscapeSequence`（`escape_test.go`）锁定上述映射，并逐例断言序列被**整条**消费
（reader 里不残留字节）。

### Windows 控制台绘制

老式控制台宿主（Windows 10 的 cmd.exe / Windows PowerShell）用 GDI 绘制，不做额外的字体回退：
控制台字体里没有的字形（例如忙图标用的盲文点阵）会被画成缺字占位框。另外它是同步重绘，先擦除
再重画会看到整块空白帧（闪屏）与跳到行首的光标。因此：

* 只有在能确认宿主自己做字体回退时才用盲文动画（`WT_SESSION` / `TERM_PROGRAM` /
  `ConEmuANSI` / `WEZTERM_EXECUTABLE`），否则忙图标退化为 ASCII 动画（`|`、`/`、`-`、`\`）；
* `LIGHTAGENT_UNICODE_UI=1` 强制使用盲文动画（老式控制台也可以换含盲文的字体）；
* 提示区原地重绘：只重画内容变化的行，行变短用 `EL`（`ESC[K`）清尾，只有行数变少才 `ESC[J`；
* 提示区每一行都被限制在「终端宽度 - 1」列内：比终端更宽的一行由编辑器自己折行（续行缩进、宽字符
  整字换行、制表符按最多 8 列计宽），窄终端放不下的灰色提示不再绘制。否则终端会自行折行，屏幕上移
  `行数 - 1` 行的数量就对不上，每按一个键都会把整行重新写一遍（表现为不断重复行）；
* 流式预览（还在到达的那一行）也按同一条限制折行，而不是截断成一行：折出的行都画在提示符上方，
  所以一行超过终端宽度时后面的文字也边收边显示；超长的一行最多占 `maxPreviewRows`（8）行，只保留
  最新的几行，提示符不会被不断往下推；
* 渲染事件对提示区的擦除是**惰性**的：只有真的写出内容前才擦除，因此「只更新预览行」或
  「没有输出」的事件不会在提示区产生空白帧，逐 token 的流式输出也不闪。

## 扩展：新增一个工具

1. 在 `internal/tools/` 新建文件，实现 `Tool` 接口（`Name`/`Description`/`Parameters`/`Execute`）。
2. `Parameters` 返回 JSON Schema（`map[string]any`）。
3. `Execute` 从 `args` 取值（可用 `stringArg`/`intArg`/`boolArg`/`hasArg` 辅助函数），
   返回 `OK(...)`、`Fail(...)` 或 `Silent(...)`。
4. 在 `main.go` 的装配处 `reg.Register(...)`（必要时在 `config.ToolsConfig` 增加开关）。
5. 在 `internal/tools/*_test.go` 增加用例，并在 `docs/tools.md` 补充说明。

## 扩展：新增一个斜杠命令

1. 在 `internal/slash/slash.go` 的 `Commands` 里加一条（名称、别名、参数提示、一句话说明；
   `Web: true` 表示网页也能执行、会出现在左侧命令栏，`Primary: true` 表示它在命令栏里默认展开
   —— 其余网页命令折叠在命令栏标题之后）。
2. 在 `internal/cli/cli.go` 的 `handleCommand` 加 `case`（`slash.Split` 已把别名和全角斜杠归一，
   所以只写主名）；需要开关参数时用 `slash.ToggleArg`。
3. 若网页也能执行，在 `internal/web/web.go` 的 `handleCommand` 加同一个 `case`：影响两边共享状态的
   命令用 `s.info` / `s.fail`（走事件总线，终端与所有网页同屏），只影响当前页面的用 `s.localInfo` /
   `s.localError`（只进网页日志区）。
4. 两侧的 `/help`（`slash.Table`）与左侧命令栏（`slash.WebCommands` → 注入页面）会自动带上新命令；
   在 `internal/slash/slash_test.go` 的命令表用例与 `docs/web.md` 的命令表补充说明即可。

## 扩展：新增一个配置项

1. 在 `internal/config/config.go` 对应结构体加字段（带 `json` tag）。
2. 在 `Default()` 给出默认值。
3. 在 `applyDefaults()` 处理缺失/非法值的回退。
4. 在 `main.go` 使用该配置；在 `docs/configuration.md` 补充说明。

## 编码约定

* 注释默认使用英文；不要改动与本次修改无关的既有注释。
* 提交前执行 `gofmt -w .` 与 `go vet ./...`。
* 尽量使用标准库；新增第三方 Go 依赖需有充分理由（目前仅 `x/text`）。
* 前端第三方库以**原样文件**内嵌（`internal/web/assets/` + `go:embed`），不引入构建步骤、
  不依赖 CDN；升级时替换文件并在该目录的 `README.md` 里登记版本与授权。
* 平台相关逻辑优先用 `runtime.GOOS` 分支；仅在必须调用平台独有的 syscall API 时
  （如 Windows 控制台的 VT 检测）才使用构建标签文件（`*_windows.go` / `*_other.go`）。
* 对外/跨包可复用的能力放在 `internal/` 各包，`main.go` 只做装配。
