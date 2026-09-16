# lightagent 文档

本目录是 `lightagent` 的详细文档。项目总览见仓库根目录的 [README.md](../README.md)。

| 文档 | 内容 |
|------|------|
| [architecture.md](architecture.md) | 模块划分、Agent 回合循环、事件总线、steering、并发模型、持久化、上下文压缩流程 |
| [configuration.md](configuration.md) | `config.json` 全字段说明、`agent.md` 覆盖、`extra_body`、配置优先级与示例 |
| [tools.md](tools.md) | 五个内置工具的参数、语义、限制与示例 |
| [web.md](web.md) | Web 镜像：启用、端口自增、登录与鉴权（加盐摘要 / 会话 cookie / 记住我）、WebSocket 协议与消息格式、`/api/config` 配置编辑（重启生效） |
| [development.md](development.md) | 构建、运行、测试、目录结构、如何新增工具/配置项、编码约定 |

## 一分钟速览

```
lightagent/
  main.go                  入口：参数解析、help/version、子命令分发
  session.go               会话装配：交互 / 一次性运行、sessions 子命令
  completion.go            shell 补全脚本
  internal/
    termcolor/             ANSI 彩色输出
    config/                config.json + agent.md
    llm/                   OpenAI 兼容客户端（流式 / 工具调用 / extra_body）
    agent/                 回合循环、steering、事件总线、上下文压缩
    tools/                 exec_command / manage_session / read_file_lines / write_file / edit_file
    store/                 单会话持久化（CWD/.lightagent/session.json）
    markdown/              Markdown → ANSI 渲染（CLI，按行流式）
    cli/                   彩色 REPL + Markdown 渲染
    web/                   WebSocket 实时镜像 + config 编辑接口（/api/config）
      assets/              内嵌的浏览器库（marked / DOMPurify）
```

数据流（一个回合）：

```
用户输入 ─▶ Agent.Submit ─▶ runLoop
                              │  ① 追加 user 消息
                              │  ② 合并 steering 消息
                              │  ③ 必要时压缩上下文
                              │  ④ 调用 LLM（流式，推送 reasoning_delta / assistant_delta）
                              │  ⑤ 有 tool_calls？执行工具 → 追加 tool 消息 → 回到 ④
                              └─ 无 tool_calls → 回合结束（保存会话、推送 turn_done）
```

事件（`reasoning_delta` / `assistant_delta` / `assistant` / `tool_call` / `tool_result` / `info` /
`error` / `compacted` / `usage` / `turn_done`）通过事件总线广播给 CLI 与所有 WebSocket 客户端；
`user` 事件带 `source`（`cli`/`web`）以区分输入来源。`reasoning_delta` 是模型思考的流式增量；
定稿后的思考写入 assistant 消息并随后续请求回传。
