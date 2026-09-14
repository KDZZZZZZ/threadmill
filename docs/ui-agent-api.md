# 本地 WebUI · Project Harness 接口

`openapi.yaml` 记录当前本地 HTTP + SSE 契约，`webui-demo.html` 是接入同源 API 的正式 WebUI 设计稿。
HTTP 网关由 `internal/cli/web.go` 适配现有 Manager；项目注册表、显示消息和事件序号目前只保存在网关进程内。
历史分页、事件回放以及跨进程 Manager 互斥尚未实现。
Manager 失败不代表所有 worker 已结束；error 项目仍可读取快照和 SSE。其 busy 只反映实际 task/model/tool/memory 活动，pending 可能保留无法处理的报告。重新打开前应确认旧运行已结束。
本轮接线在 8d40506 的普通 Tasks/Edges 上进行：角色节点含 activation；持久 task 有 idle/closed；无 SpawnedFrom/Joins/JoinedBy。graph 省略完整 Outputs，避免传播内部文件/记忆存储引用；界面采用实际节点 ID。

## Demo 已有设计（本轮按效果复刻）

- 前端部署在本地，使用 Beautiful UI。
- 以项目组织类似集群 harness 的界面；其他产品的 chat/session 在这里对应 Project。
- 同一个项目路径只有一个 session，持续复用同一个 Manager。
- 只有 Manager 与用户交流；其他 agent 的实时进度、状态可以展示。
- 先整理接口，再交付 HTML 演示。
- 工作区左侧为 Manager 对话、右侧为 Swarm，顶部使用同级英文标题；移除项目路径标题和停止按钮，logo 为短、长、短三根竖线。
- 协调图固定在右窗格上部，支持拖拽和缩放，不设中心 graph tab；运行中的节点仅让边框、图标和文字同步灰白明暗闪烁，不使用扫光，结束后停止。
- 删除 Agent 列表和独立 Thinking 区；右下 Tool Chips 汇集所有 Agent 的图标瀑布流，详情在 hover、focus 或 click 后查看。
- 活动流没有常驻细线，仅 running 活动带短尾迹；每个 Agent 列独立推进，少量活跃 Agent 居中，数量增加时压缩单元。
- 以 task 生命周期筛选 graph 和活动流；存活 task 保留各阶段的完整已接收轨迹，包括已完成的动作，只有 task 整体结束后才移除。历史 task 不显示。
- Working 加载状态始终位于最新 Manager 消息底部。
- Prompt Bar 只采用 pill 外形；Sidebar Nav 将 chat 改为 project，并允许输入指定项目路径打开 Threadmill。
- 活动详情显示来源提供的原始文本，不用概括替代。
- 正式界面不显示接口契约、UI 说明或演示开关。
- 所有界面文案、提示、弹窗和示例消息使用英文。
- 界面不使用绿色；运行状态点、节点边框和 Agent 标记使用中性灰与白色。
- 左、中、右三栏通过分隔线拖拽调节宽度；拖向边缘收起侧栏，反向拖拽可展开。
- 其他动作事件沿用 Thinking 的展开和扫光动效；进行中的事件使用纯白亮带扫过图标笔画，沿用 Beautiful UI 的 1.4s linear 节奏，结束后停止。
- Projects 标题可展开/收起；搜索在标题同行按示例过渡；打开项目弹窗需要平滑的进入/退出动效。

## 本地启动

在仓库目录运行：

```sh
go run ./cmd/threadmill -web -web-ui docs/webui-demo.html
```

默认地址为 `http://127.0.0.1:8787/`，可用 `-listen 127.0.0.1:8788` 指定其他本地端口。
页面与 `/api/v1` 由同一服务提供；启动时项目列表为空，在左栏「Open project」输入本机已有目录。
打开项目会调用真实 `manager.Open`，按配置连接模型和使用项目文件；发送消息始终交给该项目的 Manager。

开发者可用 `http://127.0.0.1:8787/?demo=1` 强制加载 fixture 预览；界面没有演示模式开关。
这些 fixture 不表示项目已在后端启动。
`-web-ui` 只指定要提供的 HTML 文件，不把项目目录作为静态网站公开。

### 通过 Tailscale 使用

网关仍绑定 loopback。使用 `-web-origin https://机器的完整.ts.net域名` 明确允许代理保留的外部 Host 与浏览器 Origin，再通过 `tailscale serve --bg --https=443 http://127.0.0.1:8787` 提供 tailnet 内 HTTPS。Windows 登录同一 tailnet 后打开该 HTTPS 地址。现有其他 Serve 端口不需要重置。

`-web-origin` 是一个精确的 `scheme://host[:port]`，不能包含路径、查询、片段、凭据或通配符；默认不允许外部 Host。代理必须在本机提供访问控制、保留 Host。网关不信任客户端自行提交的 `X-Forwarded-*` 来扩大允许范围；浏览器 Origin 仍须匹配，cross-site 请求仍拒绝。该参数不提供独立的用户认证；此部署由 Tailscale Serve 和 tailnet 访问策略控制入口。

常驻部署应由服务管理器启动网关，配置重启策略；`tailscale serve --bg` 只负责代理，不会代为启动后端进程。网关进程重启后的项目列表仍需重新打开，项目持久状态保留。

依据：[Tailscale Serve 文档](https://tailscale.com/docs/features/tailscale-serve)；其 [`ipn/ipnlocal/serve.go` 的 reverseProxy.ServeHTTP](https://github.com/tailscale/tailscale/blob/6cac918179d4d673bfebe2fc74f81183ddd73fea/ipn/ipnlocal/serve.go) 保留 HTTP 代理的入站 Host。横向检查 Pi `71dca871` 的 `packages/agent/src/proxy.ts` 和对应测试：它处理带认证的模型流代理，未提供本机控制网关的 Host 策略；Eino `9d983b36` 的 README/代码树为编排库，未发现对应 Serve 网关；本次访问 deepseek-harness 公开仓库返回404，未声称参考了不可访问实现。本次不引入新的代理库或改变 Manager 调度。

## 资源模型

```text
本地浏览器 → 本地 HTTP/SSE 适配层 → Project → 唯一 Manager
                                               ├─ 用户输入与公开回复
                                               └─ task / 角色活动（只读展示）
```

项目路径在服务端经 `Abs → EvalSymlinks → Clean` 规范化，ID 由 canonical root 的 SHA-256 派生。
网关串行化项目打开，同一网关进程内的并发请求、符号链接和路径别名复用同一 Manager。
多个浏览器窗口连接同一网关时共享该实例；断开浏览器不会关闭 Manager。
关闭后重新打开相同路径复用 Project ID、收据和仍在缓存中的消息，并启动新的 Manager 实例。
Manager 主循环失败时，Project 返回 `runtime_state=error` 和可选 `error` 原因；新消息返回 409。
显式打开同一路径会替换失败的运行实例，保留上述项目身份和缓存；已有收据重试不会再次发送。
`manager.Open` 可能恢复已有未完成工作；这属于运行时恢复行为，不属于 HTTP 消息幂等保证。

`internal/manager/state.go:openStatePaths` 将 canonical path 映射到用户目录下的项目状态目录，
`TestOpenUsesOneUserStateDirectoryForCanonicalPath` 验证路径别名共享该目录。
**持久目录复用不等于跨进程单实例锁。** 同时启动两个网关或另外运行 CLI，不受本地注册表互斥保护。
现有 Manager 状态可以由运行时恢复；WebUI 项目列表、显示消息和 SSE 游标不会因此变成持久数据。

“集群”表示项目内多个逻辑 agent 的协作视图，没有添加远程主机或另一套调度器。

## 接口清单

| 方法与路径 | 当前行为 |
| --- | --- |
| GET /healthz | 本地网关存活及服务标识 |
| GET /api/v1/projects | 当前网关的项目注册表 |
| POST /api/v1/projects | 按本机路径打开或复用项目；首次 201，复用 200 |
| GET /api/v1/projects/{project_id} | 项目、Manager 忙闲与队列 |
| GET …/messages | 进程内用户/Manager 显示消息；不支持历史分页 |
| POST …/messages | 给 Manager 发消息；返回消息 ID 和队列深度 |
| POST …/cancel | 请求体 {}；返回 {canceled}，沿用 Manager.Cancel 范围 |
| POST …/close | 请求体 {}；返回 runtime_state=closed 的 Project，不删除项目目录 |
| GET …/agents | Agent 状态和活动的只读投影 |
| GET …/graph | Manager.Snapshot 的当前协调图 |
| GET …/metrics | Manager.Metrics 的运行指标 |
| GET …/events | 当前显示快照、运行事件和 Manager 输出的 SSE 流 |

`…` 表示 `/api/v1/projects/{project_id}`。以上路由由本地 HTTP 适配层实现；
没有 `/sessions` 新建入口、worker 发消息接口或历史 revision 查询。
错误统一为 `{error:{message}}`；POST 使用 `Content-Type: application/json`。
JSON 请求上限 64 KiB，拒绝未知字段和尾随数据；网关接受本地 Host，或显式配置的代理 Host，浏览器请求须满足对应的同源检查。

## 对话与观测边界

| 内容 | 显示位置 | 规则 |
| --- | --- | --- |
| 用户输入 | Manager 对话 | messages 不接受收件人或 agent_id |
| Manager 正文增量 | 当前 Manager 消息 | 只有 manager 的 model/delta 正文进入对话 |
| 完整 Manager 回复 | Manager 对话 | 按消息 ID 替换流式正文，不重复追加全文 |
| Output 的任务报告 | Manager 对话中的可展开报告 | 由 Manager 输出通道转交，kind=task_report；默认折叠，保留全文 |
| Agent 阶段、工具名、耗时、终态 | 右窗格上部协调图、下部所有 Agent 图标瀑布流 | 只读；hover/focus/click 查看活动详情，没有 worker 输入框 |
| 上游显式提供的推理明文增量 | 图标瀑布流的活动详情 | 所有角色只读可见；reasoning_delta 与正文 delta 分开处理，不设独立 Thinking 区 |
| 未公开的内部思考、加密 reasoning、原始记忆 | 不展示 | 不能从 summary、工具事件或最终答案生成所谓原始思维链 |

`RuntimeEvent.reasoning_delta` 仅承载 provider 明确提供的推理明文，不代表可以获取模型未公开的内部思考。
摘要字段和 encrypted reasoning 不映射为 raw 文本。上游没有提供明文时，界面显示无文本状态；
不编造推理过程，也不把正文复制成推理。活动详情接收这些独立增量。

工具活动仅展示事件实际包含的工具名、调用 ID 和状态。当前运行事件不保证提供工具参数、
文件路径、命令输出或 diff；不能根据工具名生成虚构文件名或增删行数。

聊天中的用户消息、Manager正文和任务报告按Markdown渲染，支持GFM列表、表格、删除线、代码围栏等；思考与工具详情仍保留原始文本。页面内嵌固定版本的Marked与DOMPurify，原始HTML作为文字显示，解析产物经白名单清理，危险链接被移除。表格和代码块可在消息内横向滚动；按消息对象缓存结果，内容变化时重新渲染，覆盖流式未闭合语法。版本、完整许可证和校验值见[第三方记录](webui-third-party.md)。

渲染回归可用 `python3 test/webui-markdown-check.py /tmp/threadmill-markdown-check.html` 生成离线检查页，再在浏览器打开；检查页直接抽取交付HTML中的库和渲染函数，不另造一份实现。设计依据为[Marked安全说明](https://marked.js.org/using_advanced)与[DOMPurify](https://github.com/cure53/DOMPurify)。Pi `71dca871` 的 `packages/tui/src/components/markdown.ts` 及对应测试同样使用Marked并覆盖流式代码围栏；它输出终端内容，此处另外处理浏览器HTML安全。Eino没有对应WebUI，deepseek-harness仍未取得可访问实现。
图标瀑布流汇集所有 Agent 的活动；未交互时只显示图标，hover/focus/click 后查看已有详情。
每个 Agent 列独立布局；GraphTask.Outcome 为 done、failed、canceled 时整体移除任务。Outcome=active 的任务（包括 RunPolicy=held）保留各角色已接收的完整轨迹，不按动作终态筛选，也不以固定条数截断存活任务。Manager 轨迹保留到当前 busy 周期结束。task 终态同时清除对应悬浮详情。页面重载或断连时未接收的事件不能补回。
真实连接只显示有数据来源的字段；只有 Manager 接收用户消息。

`Options.Output` 目前是字符串；本地适配层识别 `[任务报告]` 前缀并设置结构化 `Message.kind`。
显示消息不是对记忆图的直接公开，也不自动带结构化 task_id。

## 实时事件与恢复

| SSE event | data 业务载荷 | 客户端处理 |
| --- | --- | --- |
| snapshot | ProjectSnapshot | 替换项目、进程内消息、agent、最近推理、图与指标 |
| runtime_event | RuntimeEvent | 更新活动；正文与 reasoning_delta 分流 |
| output | Message | 按 ID upsert，以完整内容替换同 ID 的流式正文 |

业务信封为 `{project_id, seq, data}`；仅 Manager 正文增量带 `message_id`；推理按 `agent_id` 关联。
SSE `id` 等于十进制字符串 `seq`。**seq 仅在当前网关进程、当前项目内单调递增；重启后会重置。**
消息 ID 和 `client_message_id` 收据同样不是跨重启的持久身份。
每 15 秒发送 `: heartbeat` SSE 注释保活，不产生业务 event/id。

- 每次连接发送当前快照，再发送后续事件。快照是显示层采集值，不声称 Busy/Snapshot/Metrics 的多次 Go 调用天然原子。
- SSE 不周期发送快照。当前 HTML 在真实连接时，每 1500ms 读取活动项目及 agents/graph，更新 Busy、队列和终态。
- 当前不回放历史帧，`Last-Event-ID` 和 `since` 不用于续传。重连时应接受新快照，不能用前一进程的 seq 丢弃它。
- 切换项目关闭旧连接并丢弃迟到帧；在当前连接内按 `(project_id, seq)` 去重。
- `snapshot.reasoning[agent_id]` 保存该角色最近一次模型调用的 `text/started_at/duration/running/truncated`。
  最多保留 64 KiB 的明文尾部；`truncated=true` 表示不是完整原文。旧调用与完整工具日志不回放。
- `GET …/messages` 只保留最近 256 条进程内记录，`next_before` 恒为 null；没有 before/limit 分页。
- 同项目相同 `client_message_id`、相同正文返回原收据，不再次 Send；同键不同正文返回 409。
  每项目最多 10000 个收据，满后新键返回 429，旧键仍可重试。幂等键最多 256 字节。
  该去重在网关进程内有效，不是跨崩溃的 exactly-once 保证。
- 每个 SSE 订阅最多缓冲 64 帧，满时断开该订阅，重连获取快照；单次写入超时为 10 秒，观测不能阻塞 Agent。

## 状态与实现限制

- Busy 包括排队、执行和 settling，不能把所有 worker 都标成 running。
- task 的 active 不等于某个角色正在执行；角色状态依据事件与快照，缺失时显示 unknown。
- done 是流程终态，不自动等于验证通过；project_task_id 标识真实目录归属，真实目录 task 的改动即时可见，可能包括未通过验收的改动。
- 无可验证总量时 total_steps=null，展示阶段和活动，不生成百分比。
- Go time.Duration 是纳秒；秒数为 ns / 1e9。图内 ID/TaskID/From/To 保留 PascalCase。
- Cancel 请求停止后仍需等待终态；它不会清空 FIFO，排队消息可能启动下一轮，也不是针对某个 worker 的取消 API。
- 当前 HTTP 适配层没有新增 root 产品入口或调度规则；统一边迁移仍见 unified-edge-design.md。

## Beautiful UI 组件映射

HTML 使用官方 copy-paste 源码的原生 HTML/CSS/JS 移植，沿用 dark tokens 和 MIT 来源声明；
不依赖 React 运行时或新增生产组件包。
项目导航之外，工作区为左侧 Manager、右侧 Swarm；可拖拽缩放的协调图固定在右窗格上部，活动瀑布流位于下部。

| 用户选定组件 | WebUI 使用方式 |
| --- | --- |
| Loading State / Drive | Working 的 3×3 像素动画、标签和已用时间，始终在最新 Manager 消息底部 |
| Tool Chips | 右窗格下部汇集所有 Agent 的图标瀑布流；hover/focus/click 再显示活动详情 |
| Prompt Bar / Pill | 仅 pill 外形、文字输入和发送；没有录音、模型菜单、附件菜单 |
| Sidebar Nav | Chats 改为 Projects；标题折叠列表、同行搜索（28px → 全宽，180ms）、指定路径打开项目 |

固定来源为 [Beautiful UI ff0f74d](https://github.com/slev12397/beautiful-ui/tree/ff0f74d62d8be9d89bcb735b3632e31a6ccf88dc)：
[官方接入说明](https://github.com/slev12397/beautiful-ui/blob/ff0f74d62d8be9d89bcb735b3632e31a6ccf88dc/README.md)、
[LoadingState](https://github.com/slev12397/beautiful-ui/blob/ff0f74d62d8be9d89bcb735b3632e31a6ccf88dc/components/primitives/LoadingState.tsx)、
[ToolChips](https://github.com/slev12397/beautiful-ui/blob/ff0f74d62d8be9d89bcb735b3632e31a6ccf88dc/components/primitives/ToolChips.tsx)、
[PromptBar](https://github.com/slev12397/beautiful-ui/blob/ff0f74d62d8be9d89bcb735b3632e31a6ccf88dc/components/primitives/PromptBar.tsx)、
[SidebarNav](https://github.com/slev12397/beautiful-ui/blob/ff0f74d62d8be9d89bcb735b3632e31a6ccf88dc/components/primitives/SidebarNav.tsx)、
[globals.css](https://github.com/slev12397/beautiful-ui/blob/ff0f74d62d8be9d89bcb735b3632e31a6ccf88dc/app/globals.css)。

## 横向参考

只读取接口、事件与观测相关部分，未引入这些仓库的依赖。

- Pi `713bdf3`：[RPC 设计](https://github.com/badlogic/pi-mono/blob/713bdf38d58e407db91c8d9747ea25546862b5c5/packages/agent/docs/rpc.md)、[events.ts](https://github.com/badlogic/pi-mono/blob/713bdf38d58e407db91c8d9747ea25546862b5c5/packages/agent/src/harness/events.ts)、[生命周期测试](https://github.com/badlogic/pi-mono/blob/713bdf38d58e407db91c8d9747ea25546862b5c5/packages/coding-agent/test/agent-session-runtime-events.test.ts)。参考快照与订阅边界、切换后清理旧订阅；不照搬多 session 产品模型。
- deepseek-harness `c291e79`：[Session Controller 设计](https://github.com/deepseek-ai/deepseek-harness/blob/c291e7961a515f6d7af9304e7fd1d257929aef26/packages/api/session-controller/README.md)、[ordered-baseline.ts](https://github.com/deepseek-ai/deepseek-harness/blob/c291e7961a515f6d7af9304e7fd1d257929aef26/packages/api/session-controller/src/client/ordered-baseline.ts)、[stream protocol 测试](https://github.com/deepseek-ai/deepseek-harness/blob/c291e7961a515f6d7af9304e7fd1d257929aef26/packages/api/gateway/tests/stream-protocol.host.spec.ts)。参考稳定身份与显示投影，不迁入完整 Host/Client RPC。
- Eino `9d983b3`：[callback 说明](https://github.com/cloudwego/eino/blob/9d983b36a5112a1c233056b1a099825298fafb8f/callbacks/doc.go)、[ADK callback](https://github.com/cloudwego/eino/blob/9d983b36a5112a1c233056b1a099825298fafb8f/adk/callback.go)、[callback 测试](https://github.com/cloudwego/eino/blob/9d983b36a5112a1c233056b1a099825298fafb8f/adk/callback_test.go)。参考独立观测消费者，继续沿用 Threadmill 自有 RuntimeEvent。

## Agent Self-Claimed

- 项目路由、SHA-256 项目 ID、网关内路径去重、单独关闭动作、消息身份、幂等收据和 SSE 信封。
- 使用现有 CLI/Manager 边界提供本地 HTTP 适配，注册表、消息和 seq 保存在内存；重连以快照重建视图。
- 将 provider 提供的推理明文映射为独立 reasoning_delta，区分正文、摘要和加密内容。
- 原生 HTML 源码移植、开发 fixture 模式和 Agent 只读显示投影。
- 不新增运行时业务依赖；架构与图状态的含义继续由现有 Manager 决定。

## WebUI 流故障恢复

Web adapter 为 Provider 提供可撤回显示回调；流已显示部分文本后发生瞬时错误时，先发送 stream_reset 删除本次未完成 message_id 并清空本次推理，再按配置重试。完整 Output 与用户消息不回滚；模型完整响应前没有工具执行，因此此重放不重复工具副作用。TUI/普通 stdout 未提供撤回能力时仍保持交付后不自动重放。

## 接线参考

- [Pi packages/agent/docs/rpc.md](https://github.com/badlogic/pi-mono/blob/main/packages/agent/docs/rpc.md)：查阅其标为 experimental 的服务/展示边界与快照水合设计；本实现保留现有 Manager API，只做本地 HTTP/SSE 展示适配。
- [deepseek-harness packages/api/gateway/src/stream-protocol.ts](https://github.com/deepseek-ai/deepseek-harness/blob/main/packages/api/gateway/src/stream-protocol.ts)：参考命令收据、流帧标识与显式准备状态的区分；本实现用 Project 内序号与连接首帧 snapshot，不声称有跨重启回放。
- [Eino adk/callback.go](https://github.com/cloudwego/eino/blob/main/adk/callback.go)：回调输出需要异步消费；本地订阅通过有界队列传送，网络写只在各 SSE handler，慢客户端断开不阻塞模型。
- [Go net/http ResponseController](https://pkg.go.dev/net/http#ResponseController) 与 [WHATWG EventSource](https://html.spec.whatwg.org/multipage/server-sent-events.html)：使用标准库刷新/写期限和标准 SSE framing、重连。项目 go.mod 要求 Go 1.24.2，无新增运行时依赖。

参考文件已从本机已有来源缓存核对；这里引用具体文件，未声称其 main 是固定版本。
