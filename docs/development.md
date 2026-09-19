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
go test ./... -count=1    # 全部测试
go test ./internal/tools -run Exec -v   # 单个包/用例
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
internal/store/             单会话持久化（含旧会话归档）
internal/markdown/          Markdown → ANSI 渲染（CLI）
internal/passwd/            加盐摘要：sha256(盐+密码)，Go 与页面共用同一字节约定
internal/slash/             斜杠命令表：CLI 的 /help、网页的 /help 与左侧命令栏共用（含命令解析）
internal/cli/               彩色 REPL + 多行编辑（Enter 换行、Ctrl+J 发送）
  escape.go                 转义序列解码（CSI/SS3、kitty CSI-u、modifyOtherKeys）
  term_windows.go           Windows 控制台 raw 输入（ReadConsoleInputW + Win32 input mode）
  term_linux.go             Linux termios raw 输入 + kitty 键盘协议请求
internal/web/               WebSocket 镜像 + config 编辑（/api/config）+ 登录（/api/login）
  index.html                页面骨架（服务端注入 markdown/结果开关与命令栏；含登录对话框）
  app.css                   页面样式（含配置表单控件与登录面板样式）
  app.js                    页面行为（对话镜像，连接受会话状态门控）
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
| `config` | 默认值、往返、部分配置补默认、`agent.md` 覆盖、`@include` 展开（文件/目录/嵌套/相对与绝对路径/循环与深度/整行匹配/CRLF）、校验、`-c` 指定路径、编辑器解析（`Parse`/`ParseStrict`：默认值补全、未知字段宽容 vs 拒绝、空/多份文档）、`MaskSecrets` |
| `llm` | `extra_body` 合并与覆盖、SSE 流式聚合、非流式解析 |
| `tools` | 行读取分页/EOF、写文件三种模式与行截断（截断位置预览）、编辑三种模式（`replace`/`insert`/`regex`）、GBK 往返、CRLF 保持、脚本语言选择（宿主引擎 / Python 直接引擎、`language` 的 `enum` 与默认值、工具描述里的可选值说明与 Python 版本注入、未知名报错）、子进程 stdio 的 UTF-8 模式（前置头 + `PYTHONIOENCODING` + 直通）与回退的主机代码页转换、命令快路径与后台会话 |
| `agent` | token 估算、压缩切分不孤立 tool 消息、触发判定、摘要重写（模型整篇重写，新摘要是全量更新、含旧摘要内容；指令按本次请求生成：说明报告是下次对话唯一的历史上下文，摘要在系统提示词且已有摘要时补一句替换该段摘要；摘要失败保留原摘要）、摘要放置（`agent.summary_in_system_prompt`：发送时摘要作为第一条用户消息 / 追加到系统提示词，总结请求与实时请求同布局）、steering（消息行在并入上下文的那一刻广播：末轮流式中到达则本回合续跑一轮、收尾时才到则开跟进回合，事件顺序与历史顺序一致）、消息构建（含运行时环境行：模板不含 host 信息、agent.md 提示词同样追加）、重置、上下文用量（接口 `prompt_tokens` 锚定 + 追加估算、回合中实时广播） |
| `store` | 会话保存/载入往返、存在性判断、旧会话归档（时间戳 + 冲突序号） |
| `cli` | 按键分类（Enter 换行 / Ctrl+J 发送 / Ctrl+Enter、Alt+Enter 在终端能上报时同样发送）、转义序列解码（CSI/SS3 整条读干净、kitty `CSI 13;5u`、modifyOtherKeys `CSI 27;5;13~`）、提示行布局（忙图标与彩色耗时在标签之后、光标回退到 `> ` 之后、空输入提示、实时上下文用量标签）、提示区原地重绘（只重画变化的行、缩行用 `EL` 清尾、行数变少才 `ESC[J`、惰性擦除：无输出的渲染事件不擦提示区）、提示区行宽（超宽输入按终端宽度折行、宽字符整字换行、窄终端不画灰色提示）、流式预览折行（未完成行按终端宽度换行、宽字符整字换行、超过 8 行只保留最新窗口）、待发送消息在提示区显示为灰色 `(pending)` 行（正式行随后按事件位置打印，无提示区时改打灰色提示）、忙图标字形回退（无盲文字形的控制台用 ASCII）、多行消息续行缩进、工具结果裁剪/缩进、banner 字段对齐、`/result` 开关、用量格式化、确认提示（恢复/保存）、本终端输入与其它客户端输入的消息都按事件到达位置绘制（编辑器不回显） |
| `markdown` | ANSI 渲染、围栏代码不解析、行内强调、intraword `_`、流式分块 |
| `termcolor` | 颜色开关、`NO_COLOR`/`TERM=dumb`、非 TTY 关闭 |
| `passwd` | 加盐摘要（`sha256(盐+密码)`）与 Go/JS 一致的字节约定、盐的随机性与格式校验 |
| `web` | 会话登录（`/api/session` 的盐与状态、摘要登录与错误摘要、超时 429 限流、记住我的持久 cookie、登出、401 门控与 WebSocket 握手拒绝）、同源校验（CSRF）、密码接口（立即生效、轮换盐、登出其它会话、清除后开放且文件里留下空的 `password`、`password_salt` 随之移除）、`/api/config` 读写（打码回显、保留原密钥与密码、拒绝不可启动/未知字段的文档且不动文件、写回的文件立刻可被 `config.Parse` 读回）、配置编辑器接线（Form/JSON 双模式、每个配置项都在表单里、密码控件不把明文写进文档、401 触发重新登录、控件与样式）、页面公开而数据端点受保护、回放缓冲与行顺序（按事件到达顺序记录行、消息行才画在进入对话的位置、pending 行标记与"本轮输出插在 pending 行之前"、转正式后行原位更新、运行徽标里的 queued 计数）、手机端紧凑顶栏（用量徽标只留百分比、登出为图标按钮）与输入框短提示语 |

> 含真实子进程的用例（`exec_command`）在不同平台使用不同命令（Windows 用 PowerShell，
> 其它用 `sh`），耗时约 2 秒。

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
