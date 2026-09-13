> 此 Session 草案已由根据指定 demo 整理的 [Project API](../openapi.yaml) 取代；本文件保留设计来源，不代表当前接线契约。

# UI 与 Agent 后端接口边界

OpenAPI 草案在 [`ui-backend.yaml`](ui-backend.yaml)。全部 HTTP/SSE 路由都是 **planned adapter**；当前 CLI 只有进程内接口。本次只整理契约，不实现服务，不修改 Agent 调度、工具或记忆语义。

## 已有能力映射

| UI 能力 | 当前后端接口 | 适配方式 |
| --- | --- | --- |
| 发送用户消息 | [`Manager.Send`](../../internal/manager/manager.go) | `POST /v1/sessions/{id}/messages` 入队，返回 `202`；不等待整题完成 |
| 取消前台工作 | `manager.Manager.Cancel` | `POST /cancel`；持久 task 不因 UI 取消而关闭 |
| 会话状态 | `Busy`, `ModelName`, `Close` | `GET /sessions/{id}` 与 `DELETE`；ready/closed 等注册状态由 adapter 跟踪 |
| 任务图 | `Manager.Snapshot` → [`coordination.Snapshot`](../../internal/coordination/graph.go) | `GET /graph` 将一次快照投影为固定 revision 的有界元数据页；UI 不直接改图 |
| 完整聊天与任务报告 | `Manager.Options.Output` → [`tui.OutputMsg`](../../internal/cli/cli.go) | SSE `ui_output` 和 `GET /messages` 的完整展示记录；不能只订阅 OnEvent |
| 渐进文本 | `OnEvent` 中 manager 的文本 delta | 单独的 SSE `ui_delta`；只影响临时展示，不作为完整历史 |
| 运行元数据 | `Manager.Options.OnEvent` → [`event.RuntimeEvent`](../../internal/event/event.go) | SSE `runtime` 保留 kind/phase/agent_id/call_id 等元数据，去掉 Delta |
| 指标 | [`Manager.Metrics`](../../internal/manager/metrics.go) | `GET /metrics`；含嵌套时长的全部 duration 都是整数纳秒 |
| 已发布文件 | 发布收据 + display surface | `GET /artifacts?path=...` 只读；path 是工作区相对路径，不是 files_ref |
| 持久 task 激活/关闭 | 用户消息 → Manager → 编排工具 | 首期只走 `POST /messages`，由 Manager 决定 continue_task/close_task；没有直接控制 Graph 的 HTTP 端点 |

## 消息、展示与事件恢复

- `Manager.Send` 不返回 turn ID，Output 回调也没有请求关联。客户端传 `request_id`，adapter 返回已入队用户消息的 `message_id`；它们只是传输与展示收据，不伪装成 Manager turn、task 或完成凭证。本草案不承诺重试恰好执行一次，客户端不能因超时自动重复发送有副作用的意图。
- `/events` 用 SSE framing：`id` 是会话内 adapter 接收顺序，`event` 区分 `runtime`、`ui_delta`、`ui_output`、`resync_required`，`data` 是对应 schema 的 JSON。内部 Bus 回调可并发；adapter 负责排队编号，不声称存在跨 Agent 的全局因果总序。
- `ui_output` 来自完整 Output 回调，包含 manager 回复或任务报告；`ui_delta` 只取 manager 的可展示文本，不取其他 Agent 文本或工具参数。完整 manager_reply 替换临时文本，task_report 独立追加；重放完整记录按 message_id 去重，重同步时丢弃未完成临时文本。
- 聊天正文与元数据分流：runtime DTO 永远不含 Delta；聊天记录只能进入明确的 UI history，不得顺手写入遥测日志。模型提示词、凭据和工具原始参数也不属于该流。
- adapter 回调只做有界内存入队，不在同步 Bus handler 中写客户端网络。UI delta 可合并；慢客户端被断流后用 Last-Event-ID 恢复，不能反压到 Agent。单个客户端溢出不清空其他客户端或整个会话的完整输出历史。
- SSE 游标过期时发送一次 `resync_required` 并关闭；来不及发送控制事件就断开，重连仍须检测保留缺口。客户端重新 GET session、graph 和 messages。HTTP 分页游标过期返回 `410` 与同一 ResyncNotice，其他错误用 `invalid_request/not_found/conflict/internal_error`，不把控制事件塞进 RuntimeEvent.kind。
- `GET /messages` 在 adapter 的同一接收序列上捕获完整记录窗口和 stream_cursor。取齐该窗口后，从 stream_cursor 接续 SSE。记录与重放均有容量/时间上限；旧记录不在保留范围时 `history_complete=false`，UI 明示缺失。graph/session 无法还原丢失聊天，不声称已经恢复完整历史。

## 图分页与 DTO 边界

- `Task/Node/Output` 当前 Go 结构没有小写 JSON tag，直接 marshal 会得到 `ID/TaskID/FilesRef`。API 使用明确的 snake_case DTO 映射，不修改内部类型或持久化格式。files_ref/memory_ref 只是不透明逻辑标识，禁止暴露宿主路径，也不能当文件下载路径。
- 首次 graph GET 只调用一次 `Manager.Snapshot`，按稳定顺序投影 task/node/edge/output 记录，全文 Info 只留不超过 256 字符的预览，Output.report 不进入图页。每页最多 500 条，游标绑定所捕获的 revision 和位置，最后一页省略 next_cursor；边引用的节点可能在其他页，客户端收齐同 revision 后再判断缺失。
- adapter 对快照保留设置字节、数量、存活时间上限；游标失效返回 410，不能拿新 revision 的页拼旧图。不能对每个高频事件重拉全图。当前 Snapshot 仍复制完整图和历史，此投影只控制传输与前端负担，不能据此宣称后端已具备增量图查询性能。
- 大图分页、聊天历史和 SSE 重放均是待实现的 adapter 读模型，不是新增协调图、调度规则或存储核心。首期不承诺节点全文查询或记忆导航；记忆子图的有界只读接口尚未从 Manager 暴露，保留为明确缺口。

## 会话与控制边界

- workspace 只能是服务端登记标识，不接受任意绝对路径；当前状态目录按规范化项目路径生成，因此同 workspace 至多一个活跃 Manager，重复创建返回 409，不声称能隔离同项目的多个并发会话。
- Cancel 直接映射 `Manager.Cancel` 的即时布尔回执：它取消前台工作或抢占 Manager 当前轮，不关闭持久 task。持久任务的继续/关闭、编排、帮助、发布选择与验收仍通过用户消息和 Manager/agent 工具完成。
- SSE 断线只清理该连接。显式 DELETE session 调用 `Manager.Close` 并等资源回收，会取消全部运行中的激活，包括持久 task 的当前激活；已持久化身份/快照保留，不等价于对每个 task 执行 close_task。
- artifact 读取只针对当前 display surface，query path 支持嵌套相对路径；拒绝绝对路径、目录穿越和符号链接逃逸。它描述用户现在看到的文件，不承诺等于某个不可变快照。

接入顺序：先验证单进程的两条现有回调（Output + OnEvent）、Send/Cancel/Close；再实现有界重放与固定 revision 页，最后让 TUI 的 Enter/Esc/Tab 使用此适配器，Web UI 复用相同契约。验证应覆盖无 delta 时的完整回复、delta 后完整回复去重、报告与回复交错、过期游标、慢客户端及持久激活回收。

当前不应实现的接口：任意文件写入、任意 shell 执行、直接修改协调图、直接写记忆图、暴露 provider API key。这些能力属于 agent 内部工具或服务端策略边界。
