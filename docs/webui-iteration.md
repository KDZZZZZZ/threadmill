# WebUI 接线与提示词迭代

目标：复刻指定 demo 的项目导航、Manager 对话、Swarm 图与活动流；所有交互接真实 Manager；从 GUI 提交先前三项真实任务并审计 GUI 与 Agent。以普通 Tasks/Edges 的 8d40506 为基线，复用主工作区已有 HTML/HTTP adapter，保留来源文件不动。

## 提示词实验 v7（2026-09-11，待实跑）

改前预测：移除责任细化与拆分之间的矛盾后，Manager 首轮可物化更多具备实际输入的独立责任；Planner 对同型独立输入复用契约后分片；Executor 可在新证据暴露遗漏边界时补充 delta frontier。模型实际并发及独立工作覆盖率应提高，不新增产品要求、不产生缺输入 helper、不重复派已完成工作。

只替换现有描述，保留所有权、准入、显式依赖、等待释放和固定出口语义。不按数量制造无价值任务。逐项记录三个真实任务是否触发、实际行为、反例；无触发不视为有效，行为不符则修订或撤回该替换。

测评模型按最新用户输入改为 grok-4.6，私有上游配置不入仓库。与此前 DeepSeek 测评不能归为单变量 A/B；提示词因果比较必须固定本轮模型与配置。

## 接线验收边界

HTTP/SSE 接入真实 Manager 的 Send/Output/OnEvent/Snapshot/Metrics/Cancel/Close。测试在用户消息到完整输出、角色激活状态、断连重连和慢客户端边界进行；GUI 由可见 Chrome 的指针与键盘输入完成，真实测评不得通过 HTTP 或 JS 绕过用户提交。

OpenAPI 以 docs/openapi.yaml 的 Project 模型为当前规范；此前 docs/openapi/ui-backend.yaml 的 Session 方案保留为草案来源，不能当作已实现能力。不实现另一套调度器。

## 上游容错

用户新指定 grok-4.6 并要求放宽超时/重试。模型 HTTP 默认无总时限，继续保留；新增可配置 max_retries 和 retry_interval_seconds，本轮为 60 / 15s（默认仍 5 / 1s）。只重试原有瞬时错误类别，保持已交付流不可盲目重放的边界。用本地 HTTP 连续 7 次 503 后恢复验证新预算，真实任务仍只从 GUI 提交。

静态测试旧的 38,500 UTF-8 字节总量约束与用户每角色含工具平均 10k token 的要求不等价，已移除这个字节阈值；分角色 tokenizer 估算单独记录，不能把估算当作 Grok 服务端精确计数。

## 提示词体积（真实装配的静态角色与工具 schema）

捕获 NewTeam/NewManager 装配后的 SystemPrompt 与完整可见工具 Definition，未调用远端模型。动态 hook、用户输入、任务包、记忆与历史不计入静态提示词。Grok tokenizer 未提供，以下是 o200k_base / cl100k_base 两种可复算估算，不能声称为上游精确账单。

| 角色 | o200k_base | cl100k_base |
| --- | ---: | ---: |
| manager | 3949 | 5176 |
| planner | 5581 | 7093 |
| executor | 4452 | 5668 |
| verifier | 4713 | 5843 |
| organizer | 3601 | 4409 |
| 平均 | 4459.2 | 5637.8 |

表为撤回未触发分片/Executor 自主 frontier 候选后的当前版本，包含仍在验证的 v14 发布规则。原始装配与计数在 /home/oops/evals/threadmill-webui-grok/static-requests-after-rollback.json、prompt-tokens-after-rollback.json；导出程序 prompt-export.go。工具定义与 v14 装配逐项相同，按相同 tokenizer 重算 SystemPrompt 差量。历次测量保留；运行中配置未热更新。两种估算均低于用户平均 10k 的目标，不代表动态请求也低于 10k。

## 首轮已验证故障（23:11 +08）

App 和 Research 首轮 Manager 分别 4m22.9s / 4m25.0s 收到 stream_read_error，两个运行均在创建 task 前失败；这不是观察超时或无流数据的推测。重试计数 0，原因是 Manager 正文已显示，原交付保护拒绝重放。新增仅 Web adapter 可撤回的未完成显示回调，并在上游重试前发送 stream_reset；常规 CLI/TUI 无撤回回调时保持不重放。HTTP/SSE 回归验证撤回、一次最终输出与消息去重，race 通过。ML 仍健康执行，保留 8790 原进程；修复版 8791 只重跑已失败两项到 workspace-v2，避免打断 ML。

## 提示词实验 v8：环境与交付边界（预测先记）

观察：v7 ML 的 task-1 于 23:18:38 在归档时报 VFS 总量超过 200 MiB；9m07s 的实现没有形成可消费出口。bash 描述同时要求项目相邻固定目录且禁用 /tmp，容易把可重建环境与交付混在一起。核对 exec/bwrap.go、scheduler.go runtimeDir 和 scheduler_test.go：工具提供隔离的 TMPDIR，同环境可复用，Reap 后回收，不跨角色/激活继承。

替换预测：复用规则按交付与可重建运行环境分开，依赖/缓存进入环境私有 TMPDIR；保留锁定、重建入口和原始证据，应消除该类归档超限而不削弱复现。只改 bash 原有缓存路径与收尾描述，不添加 baseline 特例或放宽 VFS。新 ML workspace-v3 用同模型/资源验证；既有运行的配置不变。若仍把环境归档或缺重建证据，撤回/修订该规则。

## v7 Manager 反例与 v9 修订预测

App workspace-v2 和 Research workspace-v2 的首次成功编排都只有一个包住整题的 task；因此 v7 Manager 的“更多独立责任首轮物化”预测没有达到，不计为有效提升。Research 的失败响应虽口头声称并行，实际重试后的图仍只有单 task；工具成功后的图才算证据。Planner/Executor 改动仍需各自行为检验。

v9 在同组现有分类/Task Info 描述中明确方法：先列可以不消费彼此成果的工作面及输入/验收；最终一个交付物不等于一个 task。共享契约缺失时，展开契约准备和不依赖它的工作，实际消费者才等待。预测：复杂应用首次有效 frontier 应出现多个独立工作面，后续集成保留唯一责任且没有缺输入 helper；简单 ML 不强迫增加 task。先用新的 app workspace-v4 验证，v7 运行不终止，结果按项目分开统计。

## 运行中的接线验证

修复版 Research 于 23:22:42 实际收到 stream_read 重试事件，后续完成模型响应并创建 task；GUI 保持 Connected，未重复追加失败半段。原 8790 ML 的修复编排请求之后也遭上游流错误，已确认 terminal，原记录保留；8791 的 ML v8 对照继续。

App v7 创建 task（23:20:54）后，compact_memory 从 23:20:58 到 23:21:48 持续 49.60s，task 在压缩完成时才启动。这是实际启动延迟证据，非执行槽位拥堵；本轮没有修改其核心调度顺序。

GUI 已修复重开项目路径自动选中、同名目录补父目录辨识；390px 无横向溢出，移动 Swarm 可打开。瀑布流改用一次分组和 ID→DOM 映射，保留原有顺序与图标交互，移除每节点重复扫描。Pending 指标包含处理中请求，界面改称 pending requests，避免错称全在队列等待。

## v8 完成证据与运行观察（23:46 +08）

ML workspace-v3 的 task-1 完成，verifier PASS 后由 manager 发布。项目含原输入共 28 文件、300820 bytes；没有归档可重建 venv，也未再发生 200 MiB 超限。入口使用 `${TMPDIR:-/tmp}/digits-baseline-venv`，保留 requirements、原始预测、指标、源文件及日志。verifier 的报告记录新建 `/tmp/verifier-digits-probe-venv` 安装后在隔离副本从公共入口执行，exit 0、899 条预测一致；复现范围明确是固定 example 与数据 + PyPI 1.9.1，未把该 commit 的 1.10.dev0 二进制构建标为通过。v8 的归档与重建预测本次得到支持，不能据此声称任意任务都不会超限。

App v7 实际两个 helper 并发，Research v7 实际三个证据 helper 并发，exec 队列没有拥堵；远未验证几十至上百并发。v9 Manager 首次响应已发生三次可撤回流重试，仍未产生有效 task，保持运行，不能用失败半段评价拆分结果。

Help 会暂停调用者，GUI 原来仍给该 executor 活跃样式。适配层和前端现将 `coordination_requestHelp` 标为 waiting；回归 `TestWebHelpWaitIsNotShownAsActiveExecution` 从失败到通过，`go test -race ./internal/cli` 通过。可见浏览器截图 gui-help-wait-v2.png 确認父 executor 等待、两个 helper executor 活跃。后端修复待下一次构建加载，运行中旧二进制由前端归一化显示。

ML 完成后的完整 task report 把用户最终答复挤出视野。改为原生 details 默认折叠，保留全文与展开状态；这是显示层改动，不改裁决或数据。

可见浏览器经滚轮回到报告后，实际点击 summary 展开（open=true 且全文含 PASS），再点击收起（open=false）；截图 gui-ml-report-top.png、gui-ml-report-expanded.png。完成后 Working 消失、project busy=false；未发现 JS 异常。只读 OpenAPI 校验与内联 JS 语法检查通过。

ML 效率账本（显式模型请求，含 Manager 收尾）：Manager 4 次模型请求、73853 total token；Planner 3 次、33011；Executor 24 次、784243；Verifier 11 次、336189。显式请求输入 1168481、输出 58815，输入+输出 1227296，已报告 cached input 759296。隐藏记忆操作另计 77599 token（输入 64424、cached input 2944），项目全部已记录模型用量合计 1304895。task 报告的 1153443 不含 Manager 和隐藏记忆操作，不能与项目总量混用。以上是累计请求用量，不是静态提示词长度或峰值上下文。缺少真实账单，不能直接换算金额。后续需区分必要独立验证与重复上下文展开，不能以删验收换取 token 减少。

## v10 Task Info 紧凑表达（预测先记）

已观察 ML 的同一要求在范围、硬约束、不得做、交付物、验收中重复，planner 输出又继续重述。v9 App 首次编排在上游约四分钟流中断后反复重试，尚无有效图；不能断言是拆分方法错误，也不能断言重复文字造成了上游中断。

此次仅替换 Manager 原有 Task Info 段落：完整要求在任务内只写一份，用局部编号关联来源与门禁；每个任务仍自包含，不能以相邻任务或不存在的记忆代替输入。保留 v9 的独立工作面方法。预测：新的 App workspace-v5 首次成功工具调用的 Info 不再多节复述同一要求，用户硬约束无遗漏，并出现真实可独立开工的工作面；输出缩短应降低传递开销，能否穿过不稳定上游单独记录，不混作因果证明。v9 原运行继续，以保留对照；若无法保真或仍重复，将修订/撤回紧凑表达规则。

## v10 反例与 v11 修订预测

App workspace-v5 首次模型响应 2m12.86s、无重试，创建两个 task；首轮 Info 分别 888/847 字符，2.20s 压缩后同时开工。但 task-2 明写 `Input: tech stack task`，无对应图边，也没有已交付输入，所以“两节点并行”不能计为正确 frontier。它还将任务概括成初始化与数据库设计，后续完整产品责任尚未得到证明。v10 的紧凑表达只得到长度证据，保真与真实输入预测未通过，必须修订。

v11 改前预测：用既有 Task Info 段落明确目标、已有输入及来源、写入面、交付/验收与结果消费方；尚未完成的其他任务不算已有输入。同步纠正既有“后续只用 provide_help”的冲突：新 frontier 用 replace_pending，固定身份下一激活用 continue_task，仅真实拆分请求用 provide_help。预期 App workspace-v6 的 ready 单元具有具体输入/共享契约或明确无依赖；必要后续消费有真实边，manager 可以直接展开新 frontier，不把所有编排推给 Help。保持要求只完整表达一次，但不允许省掉硬约束。先核对首轮真实图，再看后续消费与完整交付；未验证的部分不列为已改善。

## 上游账号限流反例（2026-09-12 00:06 +08）

Research ev-postgres 运行 29m28s 后，上游用 Responses SSE `response.failed` 返回 `gateway_concurrency_limit: Concurrency limit exceeded for account, please retry later`。原分类不认识这个真实错误码，task 直接 failed；不能记作报告验收失败，也不能据此推断账号的精确并发阈值。

最小修复将该码纳入既有限流重试，使用用户配置的间隔和预算，不加调度器或限制逻辑。真实响应形状回归 `TestResponsesGenerateRetriesGatewayConcurrencyLimit` 先失败（仅一请求），修复后应重试到 recovered 且记录 stream_rate_limit。修复尚未加载到运行中 8791 二进制；保留存活工作，待安全恢复窗口用新构建验证。当前三个场景还未证明几十/上百模型并发，账号限额是已观察到的外部约束之一。

## v12 replace_pending 操作方法（预测先记）

Research manager 尝试修复 ev-postgres 时连续把新增 task/边作为 replace_pending 的完整输入；已有 Pause/Resume 边被遗漏，运行时拒绝 `task input already started: node task-1:1:executor already started`。原始调用冻结在 research/manager-repair-00h18.json。核对 internal/coordination/pending.go：replacePendingLocked 会重建边，保留带历史 task 并不自动保留所有正在消费的 checkpoint 边；addPendingLocked 才先携带全部旧边。

v12 替换既有工具说明中的 replace_pending 段：先保留当前仍需的 pending task 与现有显式边（尤其 Pause/Resume），再加入新增 task/消费边；删除仅限明确弃用且尚未冻结的范围。预测：修复和后续 frontier 可以在其他 task 暂停/运行时追加，不再因漏边改动冻结输入而机械失败。先用现有真实 Research 的 GUI 续作消息演练这条方法，并标为会话内方法试验；这与新项目从系统提示自主执行的验证分开，不能把用户层提醒的成功冒充系统提示因果证据。核心图保护不改。

补充观测：v9 App 等待九次 stream_read 重试后终于创建 backend/frontend/integration；两个 verifier 出口指向 integration 的 executor，首轮多 owner 得到行为证据。但 frontend Info 未携带 backend 完整 HTTP 路径/响应契约，跨层一致性仍未通过。首轮实际图保存在 v9-first-graph.json。图滚轮缩放与项目切换后的 transform 恢复由可见浏览器操作验证，未发现脚本异常。

15 秒采样截至 00:18 +08：单项目显式 model.active 峰值 4、各项目同一采样点合计峰值 8；这是下界采样且不含隐藏记忆模型。00:21:35 起采样新增 memory 活动、重试和用量，分别列示，不把 memory 操作数直接当 HTTP 并发。隐藏压缩仍可能延后角色结束和后续启动，不能仅看 model.active=0 判定死锁。

## 包装后的限流与 Retry-After

00:21–00:23 又观察到 SSE `upstream_error` 携带 `too many rate-limited requests ...`、`invalid_request` 携带 `客户端 API Key 已超过 RPM 限制`，分别使前端 verifier、后端 executor、manager 等直接失败。只对这些明确的临时限流正文识别，不把任意 invalid_request/upstream_error 当瞬时错误。正负例的实际 HTTP/SSE 往返测试先红后绿。

Retry-After 使用秒数和 HTTP-date 两种形式，取服务端时间与配置间隔中的较大值，等待可被 context 取消。[RFC 9110 §10.2.3](https://www.rfc-editor.org/rfc/rfc9110.html#name-retry-after) 是解析依据。HTTP 429 和 SSE 限流均验证了等待期间取消，不会在服务端等待期内耗尽重试预算；未知模型等永久错误仍一次失败。

横向参考：Pi 的 [retry.ts](https://github.com/badlogic/pi-mono/blob/main/packages/ai/src/utils/retry.ts) 及 [retry.test.ts](https://github.com/badlogic/pi-mono/blob/main/packages/ai/test/retry.test.ts) 区分瞬时错误与账单/长期配额限制；deepseek-harness 的 retry-policy（实现 blob `f6e6175cb9f47d6f11ad2b8ff56283b34540099a`、测试 blob `cc7ebb8fa73e166db2368119887daf13faa69e8c`，通过 GitHub Git blob API 读取）把分类、次数和退避放在 provider 路由策略内；Eino 的 [retry_chatmodel.go](https://github.com/cloudwego/eino/blob/main/adk/retry_chatmodel.go) 和 [chatmodel_retry_test.go](https://github.com/cloudwego/eino/blob/main/adk/chatmodel_retry_test.go) 提供可取消的退避与重试决策回调。本轮沿用 Threadmill 的 provider 请求预算，仅补已观测分类与标准等待头，没有移植整套重试框架。


## v11/v12 首轮及恢复证据（2026-09-12 00:55 +08）

8792 / app workspace-v7 的首轮 Manager 6m52.76s 生成编排，但把边终点写成裸 task ID `lab-integrate`，工具拒绝；第二次自行改为真实角色节点后成功。最终 Info 长度 3946/2691/1765 字符，仍在来源原文、硬约束、验收和明确不做中重述同一要求，v11 的不重复预测未通过。两实现 owner 同时开工，路径、操作、会话及错误约定分别写入两侧 Info，较 v9 的前端缺路径有改善；列表成功响应的 JSON 包装/实体字段并未完整约定，不能判跨层契约全通过。两个 verifier 都连集成 planner，独立集成规划尚未前移。首轮有效图 `v12-first-graph.json`。

Manager 从 00:44:40.068 开始 compact_memory，完成后才启动 worker；此段启动等待与账号限流、有效执行并发分别统计，不混称 CPU 槽位拥堵。

Research v12 会话内试验创建 ev-postgres-retry 后，没有接回原 Resume，随后漏掉新 pending task 的完整定义而报 unknown node。原 ev-postgres 又经实际依赖被运行时重新启动，从 checkpoint 继续，00:54 前后已形成原 verifier 出口。此前“已失败且没有固定出口”仅是 00:06 的观察，不能推断它以后不会恢复。当前补做任务与原任务出现重复工作，v12 未达到完整恢复预测；尚不计为接受的提示词提升。

## 实际活动与终止状态的 UI 修复

刷新丢失正在进行的 memory 活动：SSE snapshot 已给 agent.activity=memory，前端原来仅重建 current_tool/正在流式思考。现用真实 activity 重建 memory 指示，不生成不存在的思考内容。Runtime error 项目仍订阅事件和显示存活任务，输入仍禁用。

恢复中的 failed task：实际 ev-postgres:1:verifier 已在运行，snapshot 的旧 Outcome=failed 又被适配层覆盖到各角色。回归 `TestWebRetainsLiveRoleAfterFailedTaskIsResumed` 先红后绿：有 inflight 的角色保留实际活动，未运行角色保持旧裁决；结束后恢复按固定出口/任务状态显示。协调图 Outcome 与重启机制未修改。

00:52:12 app workspace-v4 最后一个 worker 完成后，Manager 早已停止，report 仍排进 pending=1，原 Busy() 一直为真。扩展回归以 Manager 失败后收到工作报告复现，先红后绿；错误状态下 UI adapter 的 busy 只反映实际 task/model/tool/memory 活动，pending 数仍保留，不伪称报告已处理。CLI adapter 全套 race 通过。后端两项修复尚未载入 8792 活跃进程，不能把前端截图当成后端新二进制运行证据。


## 上游无通道与过载（00:54–00:58 +08）

8792 v7 的前端 executor 在一次 stream_read 重试后收到 `new_api_error: No available channel for model grok-4.6 under group group_1 (distributor)`；旧版 v5 Manager 收到同类 code 的 `system cpu overloaded (current: 98.7%, threshold: 90%)`。同一模型配置此前已成功响应，这两次是上游容量失败。仅对该 code 下已观测到的两个明确前缀放入既有可取消重试预算，未知 new_api_error 与错误密钥仍不重试。真实 SSE 形状测试先红，修复后检查恢复；运行中二进制尚未热更新。

## 提示词 v13 修订预测（改前）

v11 的“完整一次”仍被解释为多段分别完整，v12 的“保留 pending”仍导致漏任务/边；两项不接受为成功。下一次替换已有描述，不叠加警告句：

1. Task Info 收敛为边界、契约表、未验证方案三部分。逐字硬要求与可观察验收写在同一表项，不再另附整段用户原文或重复“交付/禁止/验收”章节。预测新创建 Info 的同一硬要求只出现一次且无遗漏；跨 task 所需约定各自携带。
2. replace_pending 追加从完整 snapshot 的所有 task ID 与全部 edges 出发；既有 task 用 `{id,info:""}` 保留已冻结 Info，随后加入新的任务和边。预测旧 Info 与所需边保持、首次合法恢复调用不再报因漏任务/漏边造成的 unknown node/input already started；不以重复跑同一任务冒充恢复。
3. 真正阻塞点段落明确边接到最早实际读取产物的角色，未读取它的规划/取证不等待。预测独立工作不会仅因最终集成所需输入而延后，但真实消费者的等待边仍存在。

验证优先复用已确认完全停下的 App 历史项目及其固定出口，不再新建空白同题；新构建运行，仍只经可见 GUI 打开和提交。既有 v7 与 Research 健康工作不终止；若恢复状态自动启动，先观察，不叠加发送。此项预测记录不代表已加载或已验证。


## v13 加载与当前验证边界（01:13 +08）

`/tmp/threadmill-webui-grok-v5` 从当前修改重新构建，网关 8793。GUI 打开 app/workspace-v4 前，旧 8791 实例 runtime_state=error，tasks.running=0 且 model/tool/memory.active 均为 0；其 busy=true 来自失效队列。新实例直接恢复 Manager checkpoint，未重复发送整题。旧网关仍承载其他项目，未杀进程；同一项目日志目录含重开前后的事件，指标按 gateway 分开，不把旧实例快照算入新实例模型次数。v13 静态装配含工具平均 o200k 4456 / cl100k 5638.8 token；实际新请求持续 stream_read 重试，暂无有效编排，因此 v13 未接受。

用可见浏览器重载 Research 后，界面实见 `manager · memory · Running` 与 `ev-postgres-retry:1:verifier · memory · Running`，截图 gui-restored-memory-activity.png；无浏览器脚本异常。CLI/provider race 和最终 `go test ./...` 通过，OpenAPI 12 operation 语义校验通过。

Backend v7 已落 Planner 固定出口：共享骨架后四路 api-auth/api-devices/api-reservations/api-admin，父层保留集成与跨域验收。Executor 于 01:03:16 实际 requestHelp，说明有按计划展开的行为证据；Manager 正在处理前端故障，却反复改动已开始 lab-integrate 的 Info，被图保护拒绝，Help 暂未物化。瓶颈包括 Manager 错误恢复占住单一协调回合；不能将“计划四路”计为四路实际运行，也不能全部归因于上游。

Research 主 Executor 在恢复后再次请求三份证据；Manager 通过真实新 request_id 的 provide_help 复用三个已完成固定出口，并额外消费补做 PostgreSQL 出口。该成功不证明 v12 replace_pending 修复通过。补做与原产物重叠，仍需核对输入合流如何处理同名报告。

只读记忆体积记录 memory-size-observation.json：ML 71,532 bytes / 12 个不同节点，Research 当时 432,759 bytes / 74 个不同节点，App v7 107,641 bytes / 14 个不同节点。快照内同一 ID 的重复存储不是不同语义事实，不能当 organizer 重复节点来计算；当前规模不能证明千 agent 记忆图容量。


## v13 判定与格式实验撤回（改前预测，01:24 +08）

8793 在 4 次真实重试后，01:17:02 成功 replace_pending 并发布已通过前端。比较 v13-recovery-graph.json 与 v13-effective-recovery-graph.json：原 backend/frontend/integration 的 Info 全部逐字不变，原边全部保留；新增 backend-impl 和 integration-app。完整期望态的保留方法得到这次恢复的支持，后续消费仍待验证。

同一次新 Info 又在目标、契约表与停止条件重复需求，三段式/只出现一次预测失败；新增 backend verifier 仍连 integration planner，未得到规划提前的行为证据。因此撤回本轮格式模板和角色接线的新增措辞，恢复原有目标/输入/写入/交付/验收/消费方规则及用户要求的 C/D 切分；移除去重实验的“一次”要求，不保留它作为有效优化。预期仅撤回无效候选、保留来源保真与输入语义，不声称回撤会缩短响应或增加并发。运行中 v13 实例不改，原配置保存 config-v13.yaml，原完整提示词 threadmill-v13.yaml。此前与三角色、驻留、科研/记忆方法有关的用户明确规则不作为这次格式实验回撤范围。


## 隐藏记忆重试的实时观测

长等待进一步核对 hiddenCostProvider：compact 使用非流式模型请求，重试计数原来只随 MemoryEnd 发布；Memory.active 表示整项操作存活，不能据此判断一个 HTTP 请求已经连续运行多久。此前“请求仍存活”应限定为操作尚未结束，不能排除内部重试。现增发 KindMemory/PhaseRetry，保留最终总数；Collector 在结束时只补未实时计入的重试，兼容只含结束总数的旧事件，避免双计或记到 ModelRetries。非流式/记忆内容与调度顺序不变。

TestMemoryCompactDoesNotPublishModelEvents 与 TestMemoryRetryVisibleBeforeCompletionAndCountedOnce 从红到绿，agent/event/CLI race 通过。UI 的活动详情展示观测到的重试次数与原因，重试保持 memory 分类，刷新仍可还原整理活动。运行中的 8791/8792/8793 还没有新事件生产代码，此项暂只有测试证据，待安全构建窗口验证真实上游。


## 输入整理观测缺口与真实等待（02:02 +08）

8794 / app workspace-v2 从 Manager checkpoint 恢复后，Manager 49.72s 完成，随后的 compact_memory 在 43.10s 完成。仅提供活动 sink 的请求原先仍走非流式；修复为活动 sink 也启用 SSE，实际记录 2350 个 memory stream chunks 与一次 TTFT，GUI 没有出现隐藏记忆 JSON。TestResponsesActivityOnlyStreamRetriesWithoutResettingChat 先红后绿，并验证重试不撤回当前可见 Manager 对话。

之后 task-1-repair-4 的 tasks.running=1、model/tool/memory.active 均为零。检查 assemble.go 发现 ResolveInput 和 OrganizeMemory 两个临时循环未继承 overlay.Events；所以零指标不足以证明没有模型工作。该输入进度仍在 files 阶段，8233 个路径中 8175 个位于 frontend/node_modules；它来自早期冻结产物，不是 v8 bash 规则的新输出。只查看标量与数量，不展开整份大输入。

补齐两个循环的既有事件总线，不改变输入选择、状态提交或调度。TestAssembleReportsInputStageModelActivity 四种文件/记忆 × 成功/错误情形先红后绿，在 provider 调用内验证活动数已为 1，结束后回到 0 并记录错误；coordination/agent/event/manager/CLI race 通过。已构建 v8，未向存活进程热注入，旧指标和 token 统计因漏计该阶段仅是下界。

App v7 的四个 API helper 已实际开始，单项目观测到 model.active 峰值 5，截图 gui-four-api-helpers.png；这包含等待上游/重试中的逻辑调用，不能直接等同有效推理或 CPU 并发。研究重复 PostgreSQL helper 的 compact 在约 55 分钟后耗尽 60 次重试，01:54:16 unexpected EOF；原主 Executor 随后被旧构建未识别的 gateway_concurrency_limit 拒绝。三份原始证据固定出口仍存在，收尾尚未完成。Manager 又尝试修改已完成 Resume 的输入而被拒绝，这是编排方法反例，不是核心图应放宽的理由。


## 研究收尾恢复（02:06 +08）

02:03:01 旧 Research Manager 成功添加 task-1-repair，消费 ev-sqlite/ev-postgres/ev-redis 的 verifier 固定出口，避开失败的重复 PostgreSQL 分支；该修正发生于新提示前，是原运行自行恢复的行为证据。02:04:41 Manager 随后遭 gateway_concurrency_limit 退出（不是无可用通道）。确认旧项目 runtime_state=error、busy=false，tasks.running/model/tool/memory/exec.active 全零、tracked_process_groups=0 后，GUI 在 8795 打开同一根目录，v8 构建自动恢复 checkpoint。没有再次发送原题。输入整理事件修复已包含于此构建。

GUI 恢复后能显示待执行图与 Connected，无应用脚本异常；对话历史仍为进程内缓存，新网关不会还原旧聊天列表，只显示恢复后的新事件。这是当前适配器限制，不能称为完整会话持久化。一次 computer-use 脚本错误使用不存在的 #project-root 选择器，修正为观察到的 #new-root；该错误来自操作脚本，不归为 WebUI 应用异常。


## 输入阶段与独立验收的后续证据（02:16 +08）

8794 app/workspace-v2 在 02:14:49 进入实际 Planner，随后进入 Executor；此前从 01:46:56 task start 到正式 Planner 约 28 分钟是未观测输入处理区间，不能写成已证明的死锁。8795 Research 输入整理已实际发布 task-1-repair-1:input-organizer 的 model/tool 事件（包括 memory_neighbors），说明 v8 事件接线已得到真实调用证据。Manager compact 已有 SSE 1784 个活动块。

App v7 四路 helper 的代码/测试写入面互不重叠，但共同 conftest.py 的登录夹具调用尚未进入骨架的 auth 路由。该点先记为需要观察的隐含验收依赖，没有提前认定必须串行。随后 api-reservations Executor 固定报告说明：登录优先走 auth；骨架无路由时准备真实 sessions 行，不 mock 重叠检查或事务；11 项真实 TestClient/SQLite 用例退出 0，含两个会话竞争同一时段恰好一个成功。该报告是 Executor 证据声明，仍待 Verifier 独立核验。不能因此声称整合后的认证流程已通过，也无需在已有规则可自行处理时追加提示词。


## 第一份并行实现核验与 UI 小修（02:22 +08）

App v7 的 api-auth 已形成 verifier PASS 和 task done，其他 API 单元继续核验。输入 organizer 的泳道原用通用执行器图标，现按已有 organizer 身份显示 memory 图标；可见 Chrome 重载后确认 lane title=task-1-repair-1:input-organizer、use href=#i-memory（gui-organizer-icon.png），未出现应用脚本异常。只是显示映射，不改变身份或生命周期。


## 应用 GUI 验收与旧网关退出（02:46 +08）

App workspace-v2 于约 02:29 发布最终合入 task-1-repair-4，busy=false。Verifier 用真实 API、根测试入口及不同时间输入验证内核/胶水，但将 HTML/bundle 内容作为 UI 可用性的证据；报告明确未做浏览器点选。这部分不能代替实际 UI 验收。

以交付的 ./start.sh 在端口 48140 启动，LAB_DATA_PATH 指向独立 gui-validation/lab.sqlite。真实浏览器已验证错误密码反馈、member 登录与菜单、预约 2026-09-15 10:00–11:00（记录 r-d79a210b48e846bcb9f4047dbeffb348）、同区间拒绝 OVERLAP、借出/归还、取消已有本人预约；390px 文档 scrollWidth=375，无页面横向溢出。管理员新增 GUI验收设备（d-3219df68b5cc4e8e9a1783c54c025458）、给 d-printer 添加 09-16 10:00–11:00 停用窗、读取逾期与实际操作历史；按 3D Printer 筛选并实际点击下载 CSV，下载文件 3 行且 deviceName 全为 3D Printer，归档 gui-validation/downloads/reservations.csv。

预约按钮最初无响应，检查完整桌面才发现 Chrome 原生演示密码泄露提示遮挡输入，Page.captureScreenshot 未拍到该对话框。用 Wayland wtype 关闭后按钮正常；不是页面缺陷。操作脚本的错误 selector 与 Chrome 原生提示均单独记录，不计为 WebUI JS 异常。原生 datetime-local 用真实键盘分段输入，未用 JS 设置表单状态。

8791/8792/8793 三个旧网关在 02:40:25 同时收到 context canceled 并退出，exec session 返回码全为 0，未见对应 OOM 内核记录；8790/8794/8795 仍可达。8792 最后一次 bash 只是文件列表/grep，并无停止网关命令。退出触发方尚未确认，不能断言为上游错误、OOM 或 harness crash。V7 的四 API helper 均已有 verifier PASS，父层与集成因退出 canceled；V4 backend verifier 的旧非流式 compact 被取消（16m45s，9 次重试）。保留已完成出口，后续仅通过 GUI 重新打开原项目恢复。观察脚本改为按网关记录错误，避免一个不可达实例遮蔽其余项目。


## 应用 GUI 主流程补验完成（02:53 +08）

实际页面进一步验证停用窗预约返回 UNAVAILABLE；停止并重启单独的验证服务后，原新建预约仍显示已归还，新增设备仍存在。member2 直接打开管理路径得到无权访问，自己的记录中没有 member 的新预约；不存在的设备关键词给出明确空结果。具体操作与截图、CSV 路径见外部 app/gui-validation/observations.md。未改交付源码；这些是评测者的补充验收，不冒充原 Verifier 已运行的浏览器测试。

旧 V7/V4 在新网关 8795 重开后保持取消状态，无自动续跑；分别通过 GUI 发送 recovery-after-gateway-exit.txt，说明退出事实并要求复用已有实现、固定出口完成剩余工作，未再次发送整题。两个 Manager 都已开始处理，当前提示词来自已撤回无效格式实验后的配置。


## 提示词 v14：累计阶段发布（改前预测，02:58 +08）

Research Manager 先发布 est-script，再发布未消费它的 src-catalog 兄弟快照，用户目录中的计算脚本随之消失。Manager 正确解释仍可从固定出口恢复，但可用交付发生暂时回退。coordination_publishTask 的完整快照语义不改；修订 Manager 现有发布段落，避免把及时发布理解为轮流切换互不累计的分支。

预测：首次可用阶段产物仍及时发布；已有发布时，候选需保留当前交付仍需要的已发布成果。独立兄弟分支先用简短进度报告，实际合流后再发布，不等无关任务或长期驻留任务关闭。以实际消费关系与已披露产物判断，不能靠猜测宣称包含。明确的用户版本切换/回退仍按用户请求执行。

验证目标：V4 当前已发布前端，新的 backend-continue 完成但尚未被 integration-continue 合流时，不应以仅后端快照替换前端；合流完成应交齐两者。V7/Research 辅助观察是否出现无关等待或已交付文件消失。现有 Manager 配置冻结，因此先经 GUI 发送同一通用方法做会话内试验，和未来新装配的系统提示证据分开。再次回退、无理由拖延或未触发时不接受该实验。


v14 静态装配重测：Manager o200k=3949 / cl100k=5176，五角色平均 4465.4 / 5647.2；其余角色不变。原始请求 static-requests-v14.json，计数 prompt-tokens-v14.json。均为两种 tokenizer 的估算；当前三个既有 Manager 的系统配置未热更新，新增规则在它们的用户会话层验证。

## 未触发实验回撤（改前预测，03:14 +08）

已观察 App 四个 API helper、Research 三份证据及两个收尾 helper 都是计划内的能力拆分；没有同型输入分片，也没有 Executor 根据新发现补充独立责任的实例。因此撤回 v7 的这两处候选，不把计划内 Help 误记成它们的成功。Planner 保留共享契约及提前开工条件，删除同型分片候选句；Executor 的“什么时候请求帮助”两段恢复基线的计划内 frontier 方法。用户明确要求的首轮 Manager 并行、C/D 切分、长期驻留和科研/记忆规则保留。

预测仅是回到已有的计划内 Help 边界，不宣称回撤会提高并发或降低用量。运行中配置被冻结，不能从后续旧实例行为推断这次回撤的因果效果。新装配需重新计数并解析确认。

## 调研独立验收反例（03:13 +08）

主报告与可复算脚本已发布，脚本实际退出 0，但把 SQLite 自动 checkpoint 阈值当“无饥饿上界”，把配置阈值/假设倍率延伸成磁盘或恢复边界。官方 [SQLite WAL §6](https://www.sqlite.org/wal.html) 说明大事务也会超过阈值、普通 checkpoint 不截短物理 WAL；[PostgreSQL WAL Configuration](https://www.postgresql.org/docs/current/wal-configuration.html) 说明 max_wal_size 不是硬限制、回收保留量和重放量不同。这说明既有 Verifier 检查了脚本可运行与引用映射，却没有充分检验推导成立条件。

通过 GUI 提交 review-bounds.txt，要求仅修订报告/脚本和必要来源，保留六份已交付文件，不重做三包证据、不部署数据库。此为真实用户验收反馈，不是新增系统提示词实验；后续修正不能冒充原 Verifier 自主发现。

## v14 第一个真实发布选择（03:24 +08）

App v7 的 lab-frontend-ui 已形成 Verifier PASS；Manager 处理该报告后只发进度，明确保留此前已发布后端成果，未调用前端 publishTask。只读图仍是 api-devices:1:verifier。这支持“独立兄弟快照不覆盖当前必要交付”的会话内方法；最终合流发布和不额外拖延仍待观察，不冒充新系统提示的独立对照。

03:29 +08，主要触发点 App v4 的 backend-continue 已 done、Verifier PASS，Manager 已处理报告并说明保留当前前端，没有发布后端兄弟快照。发布仍为 frontend:1:verifier，integration-continue 已开始。这支持 v14 的保留行为；最终集成发布尚待完成。GUI 证据 gui-v14-backend-completed.png，基线文件比对 publication-v14-after-backend.json。

App v7 后端四 API 合入完成后，Manager 发布 lab-backend-close:1:verifier，未等最终 lab-app-integrate；此前设备 API 基线文件没有缺失（比对 publication-v14-v7-backend-joined.json）。因此已看到该方法允许包含当前成果的阶段快照及时发布，没有把“保留成果”一律解释为等全项目完成。最终应用交付仍单独验收。

## 调研局部修订交付（03:46 +08）

task-1-bounds-repair 通过后已发布；评测者实际运行脚本退出 0。只有决策报告和脚本两文件改变，原六文件均保留，三份 evidence 包和来源清单哈希不变。报告使用的 113 个规范化来源 ID 全能映射到账本（账本单元格不带方括号，不能按整串 `[ID]` 误判缺失）。

修正版将 SQLite 阈值/阈值处算例/实际保留大小分开，PG 配置软目标与 redo 分开，Redis 倍率明标示例，cache_size 限定为实现可忽略的建议；恢复公式需要实际工作量与速率，不再给阈值导出的上界。独立算术核对：1000 帧为 4,120,032 B，2000 帧为 8,240,032 B；后者即能反驳前者为硬上界的主张。反例为官方语义推断，没有伪称跑过数据库压测或崩溃实验。stdout 与文件审计分别保存在 research/bounds-repair-stdout.txt、bounds-repair-file-audit.json。

修正版满足本次局部验收；原 Verifier 未自主拦住错误这一方法反例仍保留，不被后续人工反馈后的成功覆盖。

## 后续集成与环境能力（03:58 +08）

App v7 的 lab-app-integrate Executor 按 Planner 计划请求两个真实单元：pkg-start-readme 与 e2e-i8-http，独立写入 README/start.sh 与 tests/e2e；父层保留接线、合入和最终门禁。Manager 已物化、两 Planner 和随后两 Executor 实际并行。这仍是计划内 Help，不支持已撤回的 Executor 自主 frontier 候选。

App v4 的 integration-continue Executor 尝试浏览器核验，工具结果显示隔离环境的 /usr/bin/google-chrome 链接目标 /etc/alternatives/google-chrome、/opt/google/chrome/google-chrome 均不存在；Firefox 是要求安装 snap 的脚本。这是观测到的环境可执行能力缺口，不是应用失败，也不能用宿主 Chrome 可操作冒充 Agent 环境已有浏览器。继续观察其恢复与最终报告，评测者的浏览器操作另计。

## V4 合流发布与真实浏览器反例（04:18 +08）

integration-continue 通过并发布完整快照，原前端 27 文件均存在且哈希未变，后端与根启动/测试/说明也已交付。v14 因此形成保留兄弟成果、及时发布累计阶段、最终交齐的会话内行为证据，保留该规则；不声称新系统提示的独立 A/B 或吞吐提升。

真实 `npm start` 在宿主 Node v24.18.0 ABI137 失败：已有 better-sqlite3 是 ABI109，ERR_DLOPEN_FAILED，随后 backend SIGSEGV，launcher 退出1。缓存最初来源尚未归因；启动脚本只凭目录存在复用，未检查可加载性。用 `/usr/bin/node` v18.19.1 ABI109 执行同一入口成功，未改源码/清缓存，不把这个环境选择算作修复。

实际浏览器通过 member 登录、设备2本地09-17 10–11预约ID4、同区间OVERLAP、借出/归还、390px无横向溢出、admin新增设备ID4、审计读回。发现预约表单常驻ISO/startsAt/endsAt、冲突直接英文错误码，以及管理页显示已知后端不支持的删除按钮。经 GUI 提交 v4-user-review.txt，仅修启动缓存兼容性和用户展示，保留已通过业务；不改 harness 角色提示。可见浏览器记录 app/gui-v4-validation/observations.md。测试服务以匹配 cwd/命令/独立数据库路径的 PID 定向停止，未停止模型网关。

V7 两个集成 helper 结束后的输入记忆整理：04:06:56.297–04:15:47.306，8m51.009s；23 个 model end、51 个 tool start，已报告总用量 722402（输入692687，cached input532480）。此段用时含模型推理/上游等待，不是纯本地记忆图 CPU 时间；文件阶段不在该计数内。原始汇总 app/v7-input-memory-after-helpers.json。这是当前记忆合入仍昂贵的直接证据，不以静态提示词 <10k 遮蔽。


## v15：验收产生的缓存也要在交接前清理（改前预测，04:41 +08）

V7 完整应用虽通过启动及 API 验收，交付 ./start.sh 在真实目录退出127：.venv/bin/pip 的 shebang 保留隔离环境 /workspace；python -m pip 能运行。发布清单2949个路径中2890个是.venv。固定报告显示 pkg-start-readme Executor 清理了测试依赖，pkg Verifier 在 /tmp 验证；lab-app-integrate Verifier 明确启动前无 .venv/bin/python，执行入口后生成了它，最终发布却包含缓存。因此 v8 的直接依赖临时化在 ML 有效，但“交接前清理非交付物”未覆盖到真实验收产生的目录，不能称普遍解决。

只替换 bash 现有最后一条清理描述，明确运行交付入口也会生成非交付缓存，交接或检查点前核对本轮生成路径并清理。预测：后续 V7 修复保持完整源码/测试/证据/重建入口，最终发布不含 .venv、pytest/__pycache__ 等可重建缓存，交付启动仍能重建运行。该规则经 GUI 发给现有 Manager 做会话内方法试验；冻结的系统配置不热更新，不声称独立 A/B。仍发布缓存、误删证据或未触发即继续修订/撤回。


## 项目切换的 Working 计时倒退（04:44 +08）

V7 处理首条修复消息期间追加清理规则，切出再切回时 snapshot 总把 busySince 覆盖成最后一条用户消息时间；真实页面计时由3m52回退为3m32。仅修前端：同一次 busy 期间取已知起点与 snapshot 消息时间的较早者，idle仍清空。重载后进程内起点无法恢复的旧限制另计。用当前真实项目新增一条自然后续消息并切换验证，不改调度或API。

真实复验：在 V4 已运行25m46.6s时经GUI追加同一通用清理方法，切去V7再切回V4，Working为25m48.4s；没有重置到新增消息时间。截图 app/gui-v4-validation/clock-monotonic.png，内联JS node --check和git diff --check通过。整页重载后的历史起点仍受现有进程内状态限制。

v15 候选实际装配计数（不调用远端）：Manager3949/5176、Planner5606/7131、Executor4477/5706、Verifier4738/5881、Organizer3601/4409；五角色平均4474.2/5660.6（o200k/cl100k）。工具描述每个含 bash 的角色增加25/38 tokens；语义试验仍待最终交付。


## 运行中追加约束的编排反例（04:52 +08）

V4 Manager 传达 v15 时先改已出 Executor 的 start-native-fix Info，被 input already started 拒绝；第二次只改 acceptance-fix 但把既有输入边抄错，被 completed planner input immutable 拒绝；第三次恢复边后仍因 acceptance-fix 已启动拒绝。三次拒绝均保护原图，不是应放宽核心约束的理由。v13 的此前追加图成功证据不等于所有修改都可靠；此次属于运行中更改 Info 的新增反例。该消息暂未成功传达到V4验收任务，不能用其未清缓存直接判v15角色内方法无效。V7 初始修复契约已写清快照不含可重建缓存，继续观察。

V4 随后在对话里称“已纳入最后一次验收”，实际图 Info 仍未改变。评测者通过 GUI 指出三次拒绝与原 Info 后，Manager 才更正为未生效，停止重试冻结输入，待当前交付完成后按真实缓存缺口决定最小修订。该纠正依赖用户反馈，不记为自主遵循“工具成功前不声称已创建”规则。


## 第二个两路输入记忆成本样本（05:06 +08）

V4 acceptance-fix-1:input-organizer 从04:57:17.426到05:04:24.129，共7m6.703s，13次model end、24次tool start、451191总tokens（input430389、cached342528）；之后正式 Planner 开始。含上游时间，不是CPU计时。摘要保存 app/v4-repair-input-memory.json。两处小修的记忆合入仍有明显成本，不能从静态角色预算合格推出整体高效。


## V4 修正版实际 Node24 与 GUI 复验（05:31 +08）

acceptance-fix 已发布（40处变更）。宿主默认 npm start 从旧ABI109二进制现场自动进入rebuild，随后前后端均启动；只读probe确认Node24.18.0、ABI137、compiledAbi137、ok=true。没有手工删缓存或切回Node18，原启动缺陷在该宿主实际修复。原Verifier的Node18限制仍如实保留，这份Node24证据来自评测者。

实际页面确认设备删除按钮去除、停用引导；原预约时段409变为可操作中文、表单值保留、无常驻技术字段；390px无横溢，原验证库新增设备仍存在。验证服务定向停止。缓存清理尚在cache-handover-fix进行，最终发布还需保留性检查。


## 已确认的网关 OOM 与恢复（05:38 +08）

8795/PID135007 于05:33:37被系统OOM killer杀死（exec exit137）。kernel明确记录global_oom、anon-rss12094572kB、file-rss164kB、total-vm16336280kB。机器MemTotal16177292kB，4GiB swap几乎用尽。这次有确切OOM证据，不与02:40三旧网关exit0/原因未知混同；也不是评测者定向停止Node验证服务导致。

为继续测评，用当前源的临时诊断构建重新启动8795，GOMEMLIMIT=4GiB；仅该诊断二进制每30s向仓库外写一次pprof堆剖析，临时Go源构建后已移除，不新增产品接口或更改图逻辑。软内存目标不等于硬上限，也不能当作根因修复。经可见GUI重新打开V4与V7，二者保留active任务状态并自动恢复；未重发原题，已完成Research不重新加载。新进程计数从零开始，累计用量分析需按进程时期分段。

原始证据heap-profiles/oom-kernel.txt；剖析live.pprof；临时诊断源码副本diagnostic-build.go.txt。参考[Go GC guide](https://go.dev/doc/gc-guide#Memory_limit)。根因分配点继续调查，不能先称内存泄漏或缓存问题已解决。


## v15 V4 最终交付与重新安装验证（05:48 +08）

cache-handover-fix 在 OOM 后自动恢复、PASS，并于05:44发布。发布树65个非缓存文件与acceptance-fix基线逐个SHA256一致，没有缺失、新增或改变；node_modules/test cache为0。评测者从干净交付执行默认Node24 npm start，安装107包后真实加载ABI137并启动，实际浏览器读回原数据库设备，停用引导及移动布局保持。服务定向停止。原始对照app/v4-cache-handover-audit.json与gui-v4-validation记录。

这支持会话内通用清理方法在新建cache-handover-fix上的效果；它没有传入先前已冻结的acceptance-fix，不能冒称旧任务系统提示热更新。V7最终Verifier验收后清理仍待验证，不能仅凭专门清理任务通过而宣称所有角色均已学会。


## VFS 快照内存放大的独立复现（05:48 +08）

只调用当前公共VFS接口，不执行命令、不物化live目录：向seed写一个4MiB文件，分别建立16/32/64个基于seed的子环境。每次GC后测HeapAlloc增量为71,326,480 / 138,445,632 / 272,683,968 bytes；各次Stats.OverlayBytes均为4,194,304，LiveDirs=0。最终pprof中cloneBlob存活256.09MiB，调用链为CreateEnvironment → snapshotOverlays → cloneFiles → cloneBlob。诊断独立进程GOMEMLIMIT=512MiB，创建的临时源码/目录已移除，未改产品Go代码。

这确认当前子环境创建会复制祖先blob内容、而overlay_bytes不计baseSnapshot的资源盲区，与轻量逻辑Agent目标有直接冲突；不等同声称本轮旧进程全部OOM分配均来自此处。旧进程05:00–05:30的heap_alloc从约5.3GB增至9.7GB，overlay_bytes仅约118–129MB；缺少被杀前heap剖析，归因仍有边界。恢复进程近期存活堆小，短时分配主要为context.Store.persistLocked整图JSON序列化，不能把累计分配当存活泄漏。

原始可复跑程序、stdout、剖析保存在仓库外heap-profiles/snapshot-reproduction.go / .txt / .pprof。本次未借机改变快照隔离、协调图或调度语义；这是已复现的资源缺陷，提示词清理生成缓存只能减少载荷，不能替代存储层解决。


## Manager idle 时误显示 Thinking（05:51 +08）

浏览器实际出现Manager Thinking，但 /agents 中manager为idle、activity为空、updated_at停在05:38，只有两个子Verifier活动。renderMessages把整个项目busy当成需要thinking占位，thinking在没有trace时默认写Thinking。只改footer条件，有Manager trace才渲染思考详情；后台工作仍保留Working。模型start本来会建立trace，旧trace结束则显示Thought for。改前managerThinking=true，改后false而Working保留；界面截图gui-manager-idle-thinking-before/after.png。内联JS语法与diff检查通过，不改后端活动或调度语义。后续真实Manager模型回合还要确认思考详情正常出现。


05:57 +08，verify-ui完成、Manager真实model start后，聊天区Thinking正常出现（gui-manager-active-thinking-after.png）；随后model end显示Thought for，compact_memory单独显示Organizing memory。此前idle分支不再伪造Thinking，正反状态均实操通过。

05:59 +08，verify-start与verify-ui均PASS，lab-app-repair Executor自动恢复；父Executor从05:26 Help到子结果完成前没有model start，支持暂停等待时不占模型回合。两子任务仍用了较多角色/工具调用，这份证据不表示拆分成本已足够低。最终累计交付待完成。


## 两份证据触发大量缓存差异输入（06:04 +08）

V7两helper均PASS，父层恢复先做输入文件整理；持久化input.paths共195项，其中193项为.venv的Python字节码缓存，只有tests/e2e下2份正式证据。原始input元数据只读复制到app/v7-final-helper-input-shape.json。该阶段耗时312.439秒，35次model end、78次tool start、885059总tokens（含上游时间，不是CPU计时），之后06:03:56进入输入记忆整理。统计app/v7-final-input-files.json。

这说明只保证最后一次发布无缓存还不足以降低协同成本：缓存已先进入helper固定出口与合入差异。v15在V4专用清理任务成功，不能据此声称解决了所有内部交接；最终V7交付仍待验证。不更改当前已冻结任务，不添加未试验的新提示。


V7 最后一次输入记忆阶段 2026-09-12T06:03:56.599+08:00 至 2026-09-12T06:10:43.536+08:00，406.937秒、13次model end、34次tool start、469729 tokens；3次尝试改共同只读节点被拒后自行纠正。摘要app/v7-final-input-memory.json。随后正式Executor恢复最终门禁。


## V7 最终缓存交付与残缺环境反例（06:28 +08）

lab-app-repair已PASS并发布37文件：原33个文件无缺失，4个改变仅为start.sh/README/frontend app.js/styles.css，新增.gitignore及3份正式证据；固定发布清单无缓存，任务结束后exec进程组为0。app/v7-final-repair-audit.json。

但用户目录直接./start.sh退出1 externally-managed-environment。其旧.venv目录还保留4个不在发布清单的解释器/lib64链接，而pyvenv.cfg已无；只读probe显示sys.prefix==sys.base_prefix==/usr。venv_usable凭解释器与pip模块可执行误接受系统环境，故并未自动重建。这是新的启动有效性边界，不应绕过系统保护，也不应把根目录残余链接冒充新固定快照携带的缓存。已通过GUI提交v7-partial-venv-review.txt，只修残缺/非目标环境检测及必要验证，保留累计应用。未改harness提示词。

为并行验收已发布的界面改动，评测者另复制37个固定发布文件到仓库外clean-preview，使用独立虚拟环境与48141端口；这不代替最终./start.sh复验，不占模型验证使用的8000。


## 建图后回执重试延迟就绪任务启动（06:37 +08）

Manager于06:32:31.390成功创建fix-start-incomplete-venv（RunPolicy=enabled、Outcome=active、消费lab-app-repair完整出口），随后回执模型调用连续stream_incomplete重试，06:36时tasks_active=1但running=0。internal/manager/manager.go的AfterTurn在505行调用runReady；打开项目无pending输入时和其他task结束也会触发，但当前均无。源码与运行现象说明回合结束作为启动时机，会把建图后的上游回执等待传导到就绪工作；这不能全算作Manager未展开任务。现有并发测试证明独立task可同时运行，未覆盖回执阻塞期间是否启动。未更改回合提交/冻结时机或调度语义。

V7显示修订另已用真实浏览器在clean-preview/48141复验：原lab.db中ID4归还记录仍在，设备主标题、中文审计摘要、原始详情点击展开、390px布局均通过；服务已定向停止。临时预览首次误用lab.sqlite新seed库，已纠正并单独记录，不把它算保留历史证据。最终./start.sh残缺环境修订仍待运行。


## 运行上下文预算试验（改前预测，06:46 +0800）

Manager回执先4次stream_incomplete后400 Upstream rejected；经GUI重开仍恢复同一7消息请求并再次400。pending checkpoint为2,163,300 bytes，正文约1,031,720字符，包含两次memory、两次coordination状态及整图工具返回。可能存在请求过大问题，上游未给明确原因，不能认定模型窗口大小或把400一律算临时过载。

仅将仓库外本轮llm.context_window由1,000,000改为262,144，这是现有提前压缩机制的运行预算，不声明Grok真实最大窗口，不改角色提示、图和调度。预测：GUI重开后先触发compact_memory，削减旧状态与历史，随后完成当前Manager回合，已建修订任务得以启动。如果不缩短/仍拒绝，则撤回预算调整再调查。前值备份config-before-context-budget.yaml。


## 256k运行预算恢复结果（06:53 +08）

GUI重开后于06:46:56先触发compact_memory，39.427秒/42416 tokens完成；Manager随后4消息请求于06:48:38成功，输入332252、总332442 tokens，无新增orchestrate或重复task。两轮末尾压缩完成后，06:51:10原fix-start-incomplete-venv开始Planner，随后Executor启动。保留仓库外262144运行窗口；新soft预算促进提前压缩，但成功输入仍大于名义值，它不是服务端硬限额或真实模型窗口声明。恢复与预算调整相关，不据此断言400一定是上下文超限。

这也补全调度观测：06:32建图后，Manager回复重试、错误恢复以及压缩期间新任务未执行；06:51回合完成后才开始。该间隔包含评测者诊断与重开耗时，不能全部当作纯调度耗时。固定任务/已发布内容保留；图未直接编辑。

## 最终启动复验与 v15 结论（07:15 +08）

fix-start-incomplete-venv的Executor和Verifier分别实跑了干净目录与缺pyvenv.cfg的残缺链接目录，均PASS；07:11累计出口发布为38文件。与37文件基线比较无缺失，改变仅start.sh/README，新增tests/e2e/incomplete_venv_start_evidence.log，发布清单无缓存。原目录仍保留旧四个链接，启动前prefix=base_prefix=/usr、cfg缺失。

评测者未预先清理，直接以原gui-v7-validation/lab.db运行./start.sh；脚本自行重建目标venv、安装并监听8000。启动后prefix为原workspace-v7/.venv且与base_prefix不同。实际浏览器读回ID4的示波器已归还记录及三条中文审计，点击详情展开正常，无新的页面异常；所启动PID772521定向SIGTERM后正常shutdown（shell状态143）。原始审核app/v7-final-start-repair-audit.json，截图final-start-member.png、final-start-audit.png。

保留v15替换：V4最终65文件与V7最终38文件均无生成缓存，累计源码/正式证据保留，重建入口通过，符合最终交付的预测。证据来自会话方法传递及新消费任务；不称冻结System热更新或独立A/B。两个helper曾带入193字节码差异的反例继续记录，说明内部交接仍未普遍执行该方法，不称其消除了全部合入成本。未触发的旧候选已撤回，不另增未经试验的v16。

07:13仅把仓库外未来运行配置tools.bash.description同步为保留的v15，语义对照确认其它键不变，262144运行上下文预算保持；已冻结实例未重开。07:06普通二进制已重建为/tmp/threadmill-webui-grok-final，不含临时诊断代码，SHA/源码HEAD/提示词与页面哈希见final-build.json。内联JS语法、12个OpenAPI operationId唯一、git diff与密钥模式路径检查通过；此次没有新的Go源码改动。

07:15–07:16 API确认V4/V7 busy=false、pending=0，task/model/memory/exec进程组均为0；实际浏览器Working与pending提示消失，完成截图gui-all-tasks-finished.png。停止本轮两个只读观察器，保留可访问网关；最终1931次采样的跨项目显式模型活跃峰值9、单项目峰值5，命令峰值最高3。无几十/上百有效并发证据。最后截图的Markdown标记仍以纯文本呈现，列为排版限制，未把它隐去或归为JS崩溃。

## Tailscale Windows 访问修复（用户后续要求）

再次检查时旧8787/8790/8794/8795均拒绝连接；不能沿用上次进程存活结论。新增可选-web-origin精确外部来源，仍只绑定loopback；Host与Origin默认保护保留，忽略客户端Forwarded头。配置代理来源的HTTP回归先得到403，修复后通过；错误来源、端口、跨站、null Origin和非法配置覆盖，go test -race ./internal/cli通过。

普通构建部署在仓库外用户systemd服务threadmill-webui.service，enabled且active，Restart=on-failure，用户已有Linger=yes；Tailscale Serve后台443代理至8787，原3080配置保留，未开Funnel。HTTPS页面/health/projects为200，实际浏览器通过HTTPS重新打开应用/ML/研究三项目，均idle；SSE收到snapshot，无页面异常。Windows节点BF-202408261826在线且tailscale ping直接7ms；未实际操控Windows浏览器，不冒称该端点击已验收。配置与构建证据tailscale-access.json，截图gui-tailscale-ready.png。远程打开项目的表单改为Project path on server，避免把Windows路径误当后端路径。未改Agent提示词。

后续Windows原浏览器报告连接意外终止。服务端IPv4/IPv6 HTTPS GET均200、证书有效、服务无重启；用户Windows执行curl.exe --noproxy *后/healthz返回200。随后用户使用独立配置目录的Edge --no-proxy-server打开页面，并明确回复“打开了”。至此有Windows浏览器可用证据；问题缩小到原浏览器访问路径，未分别隔离其代理、扩展或配置，不把“新配置+直连”同时变化的试验当成某个代理软件的确定归因。未为此放宽服务端安全检查或新增公开入口。

## GUI Markdown 渲染（用户后续要求）

通过真实HTTPS GUI打开独立markdown-smoke并发送排版检查消息。修改前消息中的h2/pre code/table均为0，正文按转义纯文本显示。前端内嵌固定版本Marked 18.0.12与DOMPurify 3.4.15（npm tarball完整性已核验、许可证保存），用户/Manager/报告共享渲染函数；思考和工具原文继续保留。原始HTML显示为文字、解析HTML按白名单清理，链接隔离新标签页，宽表/代码内部滚动，WeakMap缓存同一消息未变内容。无Agent提示改动。

修改后同一真实Grok回复与用户消息各有标题/代码/表格，合计均2；实际截图markdown-after.png。390px窄屏document_width=390、chat_width=chat_scroll_width=375，没有页面横向溢出。离线检查从交付HTML抽取实际库/渲染器，10项通过，覆盖嵌套列表、引用、代码原文、表格、任务列表、安全链接、危险URL、原始HTML、缓存更新和流式每个字符前缀。截图markdown-check.png、markdown-mobile.png及记录markdown-rendering.json。实时Grok回复已完成、项目idle；未重启服务，刷新页面即可获取新HTML。

## 2026-09-12 Btrfs 与快照内存隔离迭代

Human Design：继续通过 GUI 跑任务，自主修复系统 bug、优化并行提示词，并使用 Btrfs。

- 宿主根目录为 ext4，没有空闲分区。创建仓库外独立 12 GiB 已分配镜像，挂载为 Btrfs；新 quality-workspace 的整个项目状态通过专用目录链接落入该卷。现有项目未迁移。镜像底层仍是 ext4，不能将此实验报告为原生磁盘文件系统对照。
- 通过真实浏览器鼠标/输入提交 CSV 数据质量工具任务，含 24 类规则、CLI、Web 页面、输入输出和验收。使用现有 v15 提示词，尚未修改提示词。每 15 秒记录只读运行指标，证据目录 `/home/oops/evals/threadmill-webui-grok/btrfs-round/`。
- VFS 修改前预测：只复制快照映射、共享 Store 内部不可变 blob 内容，可消除随子环境数重复复制文件字节，同时父子写入、返回的读缓冲区和调用方写缓冲区仍隔离。入口 `CreateEnvironment` / `View.Read` / `View.Write`；不改变工具、调度或图语义。
- 先运行公开入口回归测试，64 个环境继承同一 4 MiB 文件分配 268,493,928 B，超过 32 MiB 预算，失败。修复后相同场景分配 45,568 B，通过；VFS 全量测试与 race 通过。只是快照创建分配，不能外推为整个服务内存减少相同比例。
- 现有 1000 个小文件物化基准，3 次各 3 iteration：ext4 115.0–119.6 ms/op，镜像 Btrfs 76.4–81.1 ms/op。热缓存、本机当前负载、包含 Release，没有磁盘耐久性吞吐结论。后续仍需检查真实任务是否使用 reflink、汇合成本和端到端并行表现。
- Btrfs 官方文档：mkfs 支持 file-backed image；reflink 使用共享 extent 和写时复制。https://btrfs.readthedocs.io/en/latest/mkfs.btrfs.html ，https://btrfs.readthedocs.io/en/latest/Reflink.html 。
- 全仓测试发现旧提示词字面断言仍检查“成功前不得声称已落盘”，现有 v14 后正文是“成功前不声称已落盘”。仅同步测试的等价字面断言，不改提示词或行为。
- 横向参考重新读取：Pi `71dca871bc80b6bc97be37f0ca3189399d651fff` 的 `packages/coding-agent/examples/extensions/git-checkpoint.ts` 用 Git stash 保存/恢复单会话状态，不适合直接替代本系统的多环境快照；Eino `9d983b36a5112a1c233056b1a099825298fafb8f` 的 `adk/filesystem/backend_inmemory.go` 及测试使用不可变 string、写入时替换条目，支持共享不可变内容这一取舍，但没有照搬其全局文件视图。deepseek-harness 本次已可访问，读取 `packages/fs/fs-local/src/fsio.ts`（blob e07567e0e3c2b3939896ccbfaf253701475bfa91）和 index.ts（22fc6310fb22d07273e3f6ef50bdcf7c78b1e382）：宿主文件原子发布、版本检查和每目标写锁；不是每 agent 快照隔离，保留 Threadmill 的原有隔离边界。
- Btrfs reflink 实测成功；改源文件后克隆仍保持原 SHA256。ext4 上同样 `cp --reflink=always` 返回 Operation not supported。挂载使用独立 systemd mount unit 持久启用。
- 第一轮 Manager 创建 5 个独立 task，五个模型请求随后同时活跃；尾部记忆整理结束之前它们仍未运行。尚无几十并发证据，继续观察契约出口后的规则展开。

### v16 候选：减少任务说明的重复

修改前预测：替换 Manager 的 Task Info 保真句，保留相关硬要求、逐字接口、来源及验收，避免在同一 Info 多节重复。相同 CSV 任务首轮 Info 总字符数至少下降 25%，相关硬要求不遗漏，独立开工面不少于 v15 的五个。若未满足，不以主观“更简洁”保留，撤回或重写该句。单独候选项目使用更新配置；v15 已冻结的任务不改。

同时候选二进制含 VFS blob 共享及 Manager 最终答复后立即启动 ready task 的修复。后一项公开 Manager 入口测试：阻塞尾部记忆压缩，旧实现两个 task 都不能开始；修复后两者均在压缩放行前开始，Manager race 测试通过。不得将系统启动时序改善算作 v16 提示词收益。

### 聊天窗格交互（用户追加要求）

Human Design：输入区只保留 pill；优化任务报告、Thinking 等展开动效和聊天右侧滚动条。
Agent Self-Claimed：浮动 pill 高 42 px（旧底部区域 108 px），状态内嵌；聊天区域延伸到底部并为最后消息预留滚动空间。原生 details-content 的高度/透明度过渡及 reduced-motion 降级，细滚动条保留原生拖动。消息重绘直接带回已有 open 状态，并在详情恢复后恢复滚动位置，避免临时折叠的高度截断阅读位置。
验证：真实 GUI 点击报告，100 ms 时展开高度 2249.88 px / opacity .639；收起高度 543.55 px / opacity .361，均在过渡中，最终关闭；Thinking 展开同样渐变。展开报告并接收更新期间 scrollTop 198、报告顶端 415.06 均保持不变。390 px 窄屏无横向溢出，pill 宽 354、高 42；3 段 JS 语法检查通过。来源采用 MDN ::details-content / scrollbar-width 文档，无新增依赖。截图与操作记录位于 btrfs-round。
- 滚动条拖动发现并修复真实热区冲突：聊天右边界 x936，旧分栏 separator 覆盖 x932–940，吞掉滚动条右半边拖动。将展开状态热区移至 Swarm 内 x936–944；折叠时热区限制在 header，避免遮住聊天滚动条。相同 x932 拖动修复前 scrollTop 不变，修复后 748→123，聊天宽度仍 712；分栏自身 x940→900 拖动宽度 712→672，反向恢复 712，均通过真实鼠标输入验证。
- Btrfs 全量 VFS race 初次失败于三个专门测 OverlayFS 的用例：它们仅检查宿主驱动可用，但忽略 reflink 优先策略。按 Store 实际选中后端跳过专用 OverlayFS 场景后，ext4 与 Btrfs VFS race 均通过；其余快照、发布、恢复、隔离用例照常运行。

### v16 结果：撤回

首批编排 6 个 task，其中 parser、engine、web_ui 三个无外部输入；cli/web/docs_e2e 等待前序。Info 总字符 6458，v15 为 5091，未达到下降 25%；首次 ready 数 3，低于预测的 5。24 规则整包交给 engine。与预期不符，已恢复源提示词和后续加载配置，保留冻结试验与独立实验配置作证据，不再作为有效提示词发布。一次非确定性运行不能证明该句单独造成差异，但足以否决本轮预期收益。候选 GUI 首轮还出现 task ID 作为边端点的机械错误，模型自行改成 node ID 后成功。
- 候选 GUI 验证了启动时序修复：Manager 尾部 compact_memory 于 12:06:32.940 开始，engine/parser/web_ui 三个 Planner 请求于 12:06:32.975–.977 开始；尾部压缩到 12:07:23.517 才结束。三个独立分支与压缩重叠约 50.5 秒，符合系统修复预测。该收益不归因于已撤回的 v16。证据 `candidate-start-overlap.txt`。

### v17 候选：识别同一契约下的独立条目

修改前预测：替换 Manager 首次工作面划分中的“单一最终交付也可多人实现”泛化表述，教它逐项判断独立输入与可分离写入面。共享主题不自动归属同一 owner。使用共享接口已在用户请求中明确的 32 类数据导入格式任务，预测首次至少 24 个独立实现分支、写入面不冲突、明确的集成消费者；不满足则撤回。不是与 CSV 任务的耗时 A/B，不将请求内容不同造成的差异包装为提速收益。

### v17 结果：撤回

模型宣称“32 个验证器共享接口已齐、互不依赖”，实际创建 fmt_network/fmt_numeric/fmt_datetime/fmt_structured/fmt_textgeo 五组及 integrate_cli。未达到至少 24 个 ready 的预测，源提示词和后续加载配置已恢复，冻结试验继续完成。不能将任务分组数等同于硬件容量上限，也不能据这个小型条目任务证明 32-way 一定更高效；后续需将实际计算工作与角色启动/合流开销一起评估。没有把失败候选保留在生产提示词中。证据 v17-first-graph.json、v17-verdict.json。

### v18 候选：任务角色自主请求 Help

修改前预测（候选配置，不先合入源提示词）：修正把 Help 决策固定给 Planner 的现有描述。当前任务的 Planner、Executor、Verifier 均可根据自己尚未解决的问题请求帮助；Manager 使用通知中真实请求者和 Resume，不固定返回 Executor。Executor 可据新证据更新未完成 frontier，保留已确认契约和已完成工作。使用相同 32 格式任务检查实际调用是否出现计划外新 frontier、请求者归属、返回边与重复劳动。若没有出现新 frontier，只能验证已观察的行为，未验证部分不作为有效提示词保留。记录模型并发而非仅任务总数。预测原始记录为 btrfs-round/v18-prediction.json。

### Help 等待项在 GUI 切换后消失

真实 v17 页面切换后，图中 5 个 Executor 仍等待 Help，但活动区等待项为 0。根因是 reconcileActivity 只接纳 running，未恢复 waiting；元数据刷新也不处理旧等待项。统一处理两种在途状态后，真实页面恢复全部 5 项，仍以 Waiting 呈现，不冒充活跃模型。浏览器回归使用交付 HTML 和模拟 HTTP/SSE 入口：初始快照、定时刷新、重复快照、项目重订阅、单个角色恢复及取消；三种任务角色全部覆盖。旧逻辑 6 项失败、1 项通过；新逻辑 7/7 通过，浏览器无异常。证据 help-restored.png、help-check-pass.png、help-check-before.png；可复验生成器 test/webui-help-check.py。只改变展示状态恢复，无调度或工具语义变更。

### 回读人类原讨论，纠正评估方法

原文：https://chatgpt.com/share/6a904448-6700-83e8-9367-9e46da73470f （软件工程可扩展性）。普通网页提取无正文，通过公开分享页内的序列化消息读取原文；原文及提取记录保存在外部评测目录。区分人类消息与其中助手的发挥，不将整篇助手方法论归为人类逐字要求。

人类明确指出：好的抽象屏蔽不必要的变化传播，不靠猜未来功能；能验收而无需了解内部时可委派，知道如何使用且深入下层需要大量精力时必须交出去。应用到本 harness 的判断：每个任务角色只需完成本层的语义精化；能力边界、执行分片和协调依赖分别判断；集成责任不等于独自实现全部下层。简单 primitive 可直接使用，不以层数或任务数作为抽象质量。

修正评价：v17 的至少 24 个首轮任务是本次代理设定的执行展开指标，不是人类抽象理论要求。未达该预测仍维持撤回，不反向证明五组一定错误或 32 个 helper 一定高效。v18 原“出现计划外新 frontier”预测需要实际新证据触发；没遇到该情况记为未覆盖，不能将其认定为自主权无效，更不能因此恢复“只有 Planner 决定 Help”的限制。可泛化调优句仍需真实任务验证；人类明确规定的三个任务角色自主 Help 和按真实请求者返回属于应满足的语义约束。

### v19：保留完整拆分方法，修正决策归属与共享契约误判

修改前预测见 btrfs-round/v19-prediction.json。人类明确要求保留让 Agent 更会拆分的方法，不用一句话要求一百并发。保留 Planner 原有逐层承诺、能力、认知闭包、依赖、waves、合流与验收；修订 Manager 的候选来源，区分契约定义与下层实现，取消 Executor 只能按 Planner 预列清单请求的限制，让 Executor 在合流/新证据后复查剩余能力，Verifier 组织独立证据并保留裁决。三个任务角色共享 Help 的发起者语义。预测为减少把“契约未设计”当作永久串行依赖的行为，保留真正不可分的决定；不设单次固定并发数作为整套方法的存废指标。现有运行配置冻结，不把其后续表现算作 v19 结果。

### 编排回执重复报告正文

观察：v17 Manager 的一次输入达到 253,268 token。当前图与 orchestration 工具结果均包含全体 Output.Report，下一回合再注入当前图，会重复携带同一批已提交报告。修复限于 replace_pending/provide_help 回执的模型展示：保留图结构、任务说明、输出节点和文件/记忆引用，省去回执中重复 Report 正文，并标记 reports_in_current_graph=true。完整 Graph.Snapshot、PromptProjection、持久化与 Web API 的报告不删改；continue/close/publish 回执保持原行为。这仍不是全图渐进披露或千 Agent 上下文容量方案。

公开 Graph.Run → 编排工具 → 当前图回归：原工具回执 83,147 B（含 77,824 B 证据正文），修复后 1,227 B；完整证据仍可从原节点及模型当前图取得。旧测试在两个编排动作上失败，修复后通过。coordination/manager race 与 go test ./... 全量通过。该收益是固定输入下回执字节缩减，不能外推为端到端延迟或总 token 同比例下降。现有 GUI 进程尚未切换此构建。

参考：Pi 71dca871bc80b6bc97be37f0ca3189399d651fff packages/coding-agent/src/core/tools/truncate.ts 按行/字节限制展示，保留截断状态；Eino 9d983b36a5112a1c233056b1a099825298fafb8f adk/middlewares/filesystem/large_tool_result.go 与测试将超长结果存后端并提供预览；deepseek-harness c291e7961a515f6d7af9304e7fd1d257929aef26 packages/util/output-retention/src/index.ts 及其设计记录区分完整结果和工具自有展示，并显式记录省略信息。Threadmill 此处复用现有当前图作为完整正文入口，没有引入通用截断框架或新的存储服务。Pi/DeepSeek 猜测的测试路径未取到，未声称这些用例已审阅或在本地运行。

v19 静态完整请求（含工具和包装）同一计数方式：o200k 平均 4,959.2；cl100k 平均 6,199.0，最大 7,768；均为估算，不含动态任务/记忆上下文，不是 Grok 精确 tokenizer。配置断言相应更新为“不重做已确认的上层设计”和“请求者提案不是全局决定”，保留去重与收敛约束，不用旧字面断言恢复 Planner 独占决定权。

### v19 进行中的拆分与编排观察（13:07 +08）

首次提案 12 个任务，先因 edges 使用 task ID 被拒绝，随后自行改为真实节点 ID，图原子性正常。首个 pkg_skeleton 只创建两份 docstring 包标记，仍作为九条格式/CLI 分支的 Planner 入边，任务耗时 3m57s。接口已经由用户指定，这个文件准备前置没有产生新的跨边界语义；作为无效入口屏障的反例记录，不据此删除完整能力拆分方法。骨架完成后采样的单项目模型同时活跃峰值 11；不是 task.running 计数，也不是百并发结论。8 组格式、CLI、README 与最终集成有各自写入面，随后多个格式 Executor 开始请求 Help。CLI 的契约/实现划分及后续合流尚待验证。

v17/v18 多个 Help 同时等待，但 Manager 逐条处理。只读代码核对：通知通过 Loop.Enqueue 的 FIFO 回合进入，当前图 PromptProjection 不包含尚未处理的 Help 请求正文。模型不能凭当前图获取队列中其它请求的完整 units；这属于现有控制面可见性与排队限制，不能把全部低并发归因于模型没有服从提示词。本轮未改变 FIFO 或调度语义。

报告回执去重构建通过独立 8789 候选网关运行，真实 GUI 新开 receipt-logsummary 并提交日志聚合 CLI 任务。原 8787/8788 及其任务均保持运行。首轮交付后还需从 GUI 发起增量需求，以覆盖已有报告时的新编排回执；此时尚未将代码修复宣称为真实任务验证完成。

v19 CLI Planner 实际定义 records/stats/process_lines/render_html 最小共享接口，再分别委派逐行引擎与 HTML 渲染，自己保留 CLI 入口和最终门禁；与 v18 以记录结构未定为由 split:none 的观察形成对照。符合共享契约修订的行为预测，保留该方法，但不是严格因果 A/B。同一计划仍将可独立推进的 CLI 参数/写出准备留到 helpers 返回以后，违反前缀先行目标；作为局部等待反例保留，不能仅凭 Help 数量判定编排通过。原文 v19-cli-contract-plan.txt；中间判定 v19-intermediate-verdict.json。任务仍在运行，最终验收与成本尚无结论。

### 回执去重真实 GUI 覆盖（13:23 +08）

receipt-logsummary 四个独立任务完成后，Manager 经实际 orchestration 调用新增 logsummary-evidence 集成任务。本次工具回执 12,090 字符，含 12 个输出节点；重复 Report 正文为 0，原节点仍共存 27,212 字符完整报告。逐项 FilesRef/MemoryRef 与原图一致；紧随工具调用的模型请求最新 coordination 状态块仍逐字含全部对应报告。记录 receipt-live-check.json、receipt-live-tool-result.json。证明回执展示去重且证据保留，不代表全图已渐进披露或端到端耗时按该比例下降。代码在 8789 候选真实验证，稳定 8787 与既有 8788 任务继续运行；尚未更新它们的进程。

按完整 model start/end 日志另做角色分类，见 concurrency-role-peaks.json（截至 13:30 左右）：v16 model 事件峰值 6 / 任务三角色 5 / Executor 3；v17 11 / 10 / 7；v18 12 / 11 / 9；v19 11 / 10 / 6。每项是各自峰值，不可相加；model 事件另含 Manager 与输入 Organizer，独立 memory 事件未混入此表。没有几十/上百任务角色实际同时运行的证据。

合流时日志显示 input-organizer 多轮 memory_neighbors / memory_sources_of 查询，CSV 候选一次 memory_apply 前模型调用持续 4m17s。记录为记忆整理成本，尚未将其定为缺陷；不将这些模型回合耗时归给 Btrfs 文件复制。本轮已定位回执重复并完成真实验证，尚未改变输入合入的图语义。

13:42 左右 receipt-logsummary 首轮最终发布 logsummary-evidence 固定出口；原目录五个文件完整：脚本、8 项 unittest、README、样例和 COMMAND_EVIDENCE.md，项目 busy=false，GUI Working 消失。真实最终 Verifier 复跑计数/空输入/四类坏行/输入不变并给 PASS。随后通过 GUI 提交 stdin 参数 `-` 小增强，要求保留既有行为和 8 项测试，覆盖合法/坏行/空管道并再次完整发布。仍用冻结 v19 配置，未为这次增量测试改系统提示词。

### 后续用户消息的活动状态位置

stdin 增量任务的真实 GUI 显示当前 Thinking/Working 在上一条已完成答案下面、早于新用户消息。根因是 renderMessages 无条件把 footer 挂给最后一条 Manager 消息，即使其后已有用户消息。改为忙碌且最新消息为用户时，将临时活动文章放在消息序列末尾；新回复到达后仍归到该回复，完成时保留已完成的 reasoning 展开。只改变显示锚点，不改消息数据、队列或调度。

交付 HTML 的 mocked HTTP/SSE 浏览器用例修改前 4 FAIL / 2 PASS，修复后 6/6 PASS；原 Help 恢复 7/7 仍通过，所有内联 JS 语法检查通过。在真实 GUI 请求“继续完成 stdin 增强，简短说一下现在的进度即可”后，页面顺序为旧 Manager 回复 → 新用户消息 → 唯一当前进度文章，无重复 Working、无浏览器异常。截图 receipt-live-followup-order.png；回归 test/webui-message-order-check.py。临时 8871 测试服务已停止，真实任务仍运行。

横向核对：deepseek-harness c291e7961a515f6d7af9304e7fd1d257929aef26 packages/api/session-controller/src/client/sessions/assistant-stream.ts 将 transient 帧与 attempt/settlement 区分并按序折叠；本项目此次没有搬入其事件协议，只修复本地活动锚点。Pi 在固定树与代码搜索中未找到对应 Web 消息组件，未声称审阅其该部分；Eino 9d983b36a5112a1c233056b1a099825298fafb8f flow/agent/multiagent/host/types.go 提供消息 stream API，但没有可复用的前端显示锚点实现。这些上游相关测试本次未取得，不声称运行过。

13:52 v18 的 38 个局部任务全部完成时，GUI 仍有多条报告通知等待，已发布 CLI 快照明确声明不含校验器和最终验收。日志复核：13:52:06 Manager 返回创建最终集成的工具调用，未等通知队列全部清空；说明报告队列不构成“必须全部读完才能建消费者”的硬约束。将结果就绪至消费者实际启动的延迟与单条 Help 请求的 FIFO 可见性分别记录，不能混为同一瓶颈。

### 临时输入循环遗漏配置中的工具说明

实际日志出现 input replace/finish 缺少 reason、输入 Organizer 尝试改只读共同节点等错误。装配检查发现文件合入循环直接使用原始 FileTools/Bash/input，输入 Organizer 直接使用隔离 MemoryTools，两者均未加载 YAML 工具说明。共同节点只读等规则已在输入 Organizer 的系统说明中，因此不能把所有调用错误都归因于此次遗漏。

修改前预测：在不改变工具集合、schema、实现、环境绑定、文件合入与记忆事务语义的前提下，两个临时阶段的模型请求应收到已配置的工具说明。复用现有描述包装器，输入 Organizer 仅在其隔离的 memory 工具集合上覆盖文本，不继承调用方工具或 hooks/checkpoints；未增加提示词内容。

TestAssembleConfiguresInputStages 在实际 Assemble→角色→Provider 请求边界检查 input / memory_apply 描述，并确认 memory 阶段不会因 catalog 包含 bash 而获得它。files/memory × 成功/上游失败四个分支修复前全部失败，修复后通过，原模型活动计数断言仍通过。agent/coordination/manager race 和 go test ./... 通过。证据为 input-descriptions-before.txt、input-descriptions-after.txt、input-descriptions-race.txt、input-descriptions-all.txt。新构建 threadmill-input-descriptions SHA-256 为 377ffe1450fffbe4c2b18d214c34b9b8891b3575154d99282795272b201b8e82；截至本条记录尚未替换运行中的网关，也未证明真实模型的错误率或总耗时下降。

横向核对：Pi 71dca871bc80b6bc97be37f0ca3189399d651fff 的 packages/coding-agent/src/core/tools/tool-definition-wrapper.ts 转换时保留 description、parameters 与执行上下文；Eino 9d983b36a5112a1c233056b1a099825298fafb8f 的 adk/agent_tool.go 和 TestAgentTool_Info 验证配置的名称/描述进入模型工具信息；deepseek-harness c291e7961a515f6d7af9304e7fd1d257929aef26 的 packages/core/tools/README.md 与 src/index.ts 将注册定义按当前作用域投射到可见 schema。Threadmill 复用已有包装，不引入它们的注册系统。只取得 Eino 的对应测试，未声称运行上游测试。

### 上游错误与仍待验证的方法冲突

v18 Manager 于 13:52:59、v19 Manager 于 14:09:23 收到上游 400 Bad Request；错误体只说 Upstream rejected the request，尚不能确定请求大小或其他原因。v18 没有活跃子任务时，经 GUI Open project 重新打开同一路径，14:15:24 从保存的请求恢复。v19 子任务仍工作，暂不重新打开以免中断它们。

保留 v19 全部详细拆分方法。另发现 Planner 输出模板要求“先调用 Help”，Executor 开工段要求“立即 Help、此前只做机械校验”，与各自“先完成或委派独立前缀”的规则冲突。已有 CLI 延后独立前缀的反例支持下一轮只消除此处冲突；尚未修改或宣称其效果，不以固定并发数字替换方法。

### 14:21 输入说明候选上线与真实文件保全回归

receipt-logsummary 的 stdin 增强通过 11 项单测，但发布删除了原有 COMMAND_EVIDENCE.md，与“包含原有文件的完整更新”冲突。Manager 将旧实现、旧测试、旧 README 各自作为三条分支基线，最终消费者没有消费此前完整发布快照中的证据文件；本地 PASS 不代表累计交付保全。

在 8789 全部项目 busy=false、pending=0 后，原子替换其候选二进制并重启，仅上线工具说明加载修复，提示词仍为 v19。经 GUI 请求恢复原始证据文件而保留 stdin 增强。Manager 正确把第一次完整发布与当前完整发布接到恢复任务，并限定旧快照只提供原始证据文件。文件阶段 11 次 input 调用均无工具错误且进入 memory 阶段；只说明该次协议调用通过，不外推为错误率或总耗时改善。

随后 Planner 实读发现 test_logsummary.py 缺失。保存的 input 决策表明：旧来源只 apply 了 COMMAND_EVIDENCE.md，当前 stdin 来源直接 discard；理由声称其余文件“保持当前基线”。输入协议实际以所有来源的完整状态交集开局，不自动选某个来源为基线。同名而内容不同的文件仍须显式采纳；这个操作理解错误与工具参数缺失分别记录，不能凭“input 调用无错误”宣告语义合流通过。证据 receipt-repair-input-decisions.json；发布目录仍是上次五文件版本，尚未发布这份缺文件草稿。

### 合入记忆的自动 ID 与隐藏共同节点冲突

重复出现的 `common node "mem-1" is read-only` 不能直接推断为模型请求改共同节点。确定性回归复现：输入 Organizer 的可见范围排除无关共同节点 mem-1/mem-2；合法的 memory_apply create 省略 ID 时，原分配器只检查差异子图，重新分配 mem-1，随后 Compose 误判为改写只读共同节点。

在输入 memory_apply 包装中，仅为省略 ID 的 create 分配未被完整共同部分、当前草稿及同批显式 create 占用的编号；其他参数保留原始 JSON 字段。共同内容仍不进入模型或读工具范围，不改共同节点保护、原子提交、普通图分配规则和依赖语义。回归覆盖多个自动创建、同批显式编号保留、显式状态保留及无关共同内容不泄露；旧代码合法创建失败，修复后通过。agent/coordination/context/tool race 与全仓测试通过。input-node-id-before.txt、input-node-id-after.txt、input-node-id-race.txt、input-node-id-all.txt 为证据；候选 threadmill-input-node-id 尚未部署到运行项目，不声称实际耗时下降。

Pi 71dca871bc80b6bc97be37f0ca3189399d651fff packages/coding-agent/src/core/session-manager.ts 的 generateId 对完整 byId 索引查重，而 buildSessionPath 单独选择可见分支；参考的是身份占用范围和可见范围分离。Eino / deepseek-harness 的定向搜索未取得直接可比的记忆差异图分配实现，不把无关身份生成模块当作参考；本次没有运行上游相关测试。

### v20-prefix：只改两处暂停顺序的冲突描述

修改前预测为 v20-prefix-prediction.json，旧源码为 v20-prefix-before.yaml。保留五角色的完整方法；只替换 Planner 输出模板与 Executor 开工段中“立即/先调用 Help”的表述：可独立委派的工作并入同一 wave，短小且属于调用者的前缀先做，安排好后再暂停，只把真正消费帮助结果的工作留到 Resume。同步修改原先锁定冲突旧句的配置测试。五角色完整静态请求同一 JSON/tokenizer 口径重测：o200k 平均 4,977.2；cl100k 平均 6,226.8，最大 7,830。未把估算视为 Grok 精确计数或动态上下文预算。

14:34 后 v19 只有等待的 CLI 与集成，模型/记忆调用均为零。先保存图和配置边界，再经 GUI 重新打开错误项目以让后续装配使用 v20-prefix。实际恢复却把两个等待任务标成 canceled；42 个已完成出口保留。随即经 GUI 明确原目标未取消，要求复用已验收格式，只继续剩余交付。记录为 WebUI/Manager 错误恢复的生命周期缺陷；不声称这次是无损恢复，也不把此后的变化算作纯 v19 结果。v18 在 14:15 重开同样从当前共享配置重新装配，故其此前 v18 证据与此后混合配置的恢复阶段分开；14:27 再次在 8 次 stream_incomplete 重试后收到上游 400，尚未恢复。

v20-prefix 的适用集成计划/执行行为仍待模型恢复后观察；不以配置检查通过代替行为验证。大请求是另一个限制：v18 失败 checkpoint 的消息正文估算 o200k 549,596 / cl100k 639,509 token，含约 46 万字符工具回执和约 46 万字符当前图，尚未计额外工具定义及回放字段；配置窗口为 262,144。不能据上游的泛化 400 文案断言唯一错误原因，但不能把这样的请求压力归给平均 6.2k 的静态角色提示词。

### 自带样例的浏览器流程失败

quality-v16 发布后，以真实浏览器选择 examples/sample.csv，粘贴 examples/rules.json，添加“部门”参考表并选择 examples/ref.csv，点击提交得到名称非法错误。页面限制不兼容其自带样例；已通过 GUI 请求修复并实际验证两种下载，任务 web_ref_name_repair 运行中。浏览器文件选择经过点击文件输入、原生 file chooser 事件和 CDP 文件选择响应完成，未用脚本改表单状态或直接调用业务 HTTP API代替点击。现场 quality-v16-chinese-reference-rejected.png。

### v21-input：把合流的文件选择方法说清楚

修改前预测于 14:44:41 保存为 v21-input-prediction.json，旧文本为 v21-input-before.yaml。只改写现有 input 工具说明的开头和 finish 步骤：共同部分是所有来源完全一致的文件状态，不是同名路径或自动选定的基线；要保留的差异也须明确采纳；finish 前核对当前草稿的实际文件。协议、工具参数和隔离范围不变，五角色详细拆分方法不删减。

独立 config-v21-input-only.yaml 以 v19 为基底，仅覆盖这份 input 说明，避免把 v20 的暂停顺序改动混入归因。计划待当前恢复尝试空闲后，由 GUI 在相同两个固定来源上重试，比较六份文件的原始哈希及普通路径/stdin 行为。现有五份产物和历史 COMMAND_EVIDENCE.md 的字节期望已保存为 receipt-preservation-expected.json。配置及全仓测试通过；此处尚未上线或取得行为验证。合并两个候选的静态请求估算平均 o200k 5,035.4 / cl100k 6,303.6，最大 cl100k 7,958，不含动态图与记忆。

14:55 核验第一轮恢复：任务耗时 29m17s，Executor 明示四文件缺失、拒绝按记忆重撰；Verifier 实读目录并运行公共入口，结论 FAIL。图中的 outcome=done 只说明本次激活结束，不能据此把产品判为通过。Manager 仍处理报告，发布指针仍是原 stdin-evidence 五文件版本，缺文件草稿没有发布。截图 receipt-repair-completion-check.png。v19 上游恢复则在 10 次 stream_incomplete、15m59s 后于 14:50:43 再次收到泛化 400；v20-prefix 尚无适用行为证据。

14:59 候选隔离：v20 的目标恢复未得到适用执行轨迹，v21 的原项目则于 14:58 自行创建 restore-stdin-products 再次修复，尚无安全重载空隙。为不把未验证改写留为默认，将源码及后续共享加载恢复为 v19；其完整逐角色拆分、三角色自主 Help 方法保留。两组候选独立配置、组合源码和原预测仍保存在仓库外评测目录，可继续试验；不重启正在运行的循环，也不把未覆盖当成已证伪。恢复前记录 unvalidated-candidates-isolation.json，配置测试的两处候选字面断言同步恢复。

### 新观察：包装在 upstream_error 中的中途断流没有重试

15:05:34 v17 Manager 收到 `response.failed / upstream_error / OpenAI stream disconnected before completion: filter Grok Responses billing ping: unexpected EOF` 后直接终止，重试次数为零。分类仅识别该错误码中的临时限流文案，漏掉这次明确的上游流中断；未把任意 upstream_error 或 400 当成瞬时错误。

在已有 Responses 公共 Generate→HTTP/SSE 回归表中加入真实断流形状及“invalid request document: unexpected EOF”反例。旧实现仅发一请求即失败，修复后恢复到第二次请求的完整输出，反例仍一次失败。仅补错误码+已观测前缀+EOF 后缀分类，沿用原重试预算、间隔、取消、已交付文本重放保护；重试事件归为 stream_read。证据 upstream-disconnect-before.txt / upstream-disconnect-after.txt。尚未部署，不能声称已恢复 v17。

横向复核本地保存的官方实现与对应测试：Pi packages/ai/src/utils/retry.ts / test/retry.test.ts 把未到终结事件的 Responses 流纳入瞬时错误，配额/账单另行排除；其笼统 billing 排除不能直接套在此次 billing ping 的传输诊断上。Eino adk/retry_chatmodel.go / chatmodel_retry_test.go 使用重试决策与可取消退避，覆盖永久错误一次失败及等待中取消。deepseek-harness packages/llm/llm/src/retry-policy.ts（blob f6e6175cb9f47d6f11ad2b8ff56283b34540099a）及 tests/retry-policy.spec.ts（cc7ebb8fa73e166db2368119887daf13faa69e8c）将 TRANSPORT 与次数/退避归属 provider 策略。未移植这些框架或运行上游测试，本次不修改任务图或 Manager 恢复生命周期。

provider/CLI race 与最终全仓测试通过，git diff --check 通过。新构建 threadmill-upstream-disconnect 同时包含输入说明加载、输入自动 ID 修复与断流分类，构建指纹和测试文件记录在 upstream-disconnect-build.json；尚未替换运行服务。第二轮文件恢复仍为 v19：正确 apply 历史 COMMAND_EVIDENCE.md 和当前四份差异产物，拒绝双方互删，进入记忆阶段；来源已换为第一次失败出口与 stdin 出口，不能视为 v21 的同源对照。

### 中文参考表缺陷经可见浏览器闭环

web_ref_name_repair 的 Verifier 给出 PASS，Manager 于 15:11 左右发布该固定出口。重新启动仓库外的本地演示服务后，使用同一可见 Chrome：原生选择 examples/sample.csv、在 textarea 选择全部并粘贴 examples/rules.json、点击添加参考表、输入「部门」、原生选择 examples/ref.csv、点击提交。页面 HTTP 200、错误区隐藏，总行数 3、问题数 21。随后真实滚轮滚到下载按钮并分别点击 JSON/CSV；下载文件 21 条记录逐字段一致，外键问题指向第 2 行部门编号 D9 不在「部门」表。没有通过直接业务 HTTP 请求代替这次流程。

修复前发布目录的 31 个文件全部保留，只变化 README.md、csv_dq/static/app.js、csv_dq/static/index.html、tests/test_web.py。浏览器没有脚本错误。证据 quality-v16-before-repair-files.json、quality-v16-native-gui-verification.json、quality-v16-native-submit-pass.png、quality-v16-native-results.png、quality-v16-native-download-buttons.png 与 quality-v16-native-downloads/。这次产品修复和 GUI 验收通过，不代表冻结 v16 提示词恢复有效；其原先未达预测而撤回的结论不变。

quality-workspace 基线的 integrate_repair 随后通过并发布。可见浏览器原生选择中文正例 CSV 和规则文件后显示 2 行/0 问题；换反例显示姓名必填问题；保留所选规则文件、在编辑区把必填列改为城市后变成 0 问题，改回姓名恢复为 1。分别点击下载 JSON/CSV，记录逐字段一致。现场 quality-baseline-native-chinese-pass.png、quality-baseline-native-problem.png、quality-baseline-native-downloads.png，数据 quality-baseline-native-gui-verification.json。只覆盖这些实际入口与编辑/下载路径，不据此声称逐条浏览器覆盖全部 24 类规则。

### 根目录 listing 缓存漏依赖，让存在的文件从输出中消失

第二轮恢复 Executor 报告四文件可读且哈希正确，却为补齐目录项又经 TMPDIR 复制同样字节。只读追溯实际 cmdcache：`ls -la` 的旧回执只有 events.jsonl，reads 只记录 events.jsonl 类型和 security.selinux 不存在，未记录根目录条目；其它 `ls -la /workspace` 条目同样缺根依赖。原证据保存为 receipt-root-listing-cache-entries.json。早先记忆里“sha256sum 成功而 ls 缺文件”的矛盾因此有具体工具层反例，不应直接判成 VFS 字节丢失或 agent 没按提示合流。

公开 Scheduler/Store 回归：agent-a 执行并缓存 `ls -1`，agent-b 新增 new.txt 后执行同命令；旧代码仍返回 README.md/input.txt，漏 new.txt，先红。根因是 trace.workspaceRel 对工作区自身返回空串，被 record 跳过。统一映射为 `.` 后，根目录沿现有 ReadDir/条目摘要参与校验；新增文件时重跑，未变目录仍可复用。ParseTrace 同时覆盖 `/` 和 `/workspace` 两种根。未新增整树内容摘要、调度或 VFS 依赖。

旧缓存缺失的依赖不能事后猜出，索引命名空间从 tmcmd1 升到 tmcmd2，保留旧文件但不再命中它们。旧条目夹具的公共 Lookup 先复现错误重放，修改后拒绝；原 cmdcache/exec 全量用例通过。证据 root-listing-cache-before.txt、root-listing-cache-after.txt、root-listing-legacy-before.txt、root-listing-cache-suites.txt。修复范围是根目录条目变化，不宣称 stat 类型依赖已精确覆盖所有时间戳/权限元数据。

横向核对：Pi `713bdf38d58e407db91c8d9747ea25546862b5c5` 的 [bash.ts](https://github.com/badlogic/pi-mono/blob/713bdf38d58e407db91c8d9747ea25546862b5c5/packages/coding-agent/src/core/tools/bash.ts) / tools.test.ts 与 DeepSeek `c291e7961a515f6d7af9304e7fd1d257929aef26` 的 [bash-local/src/index.ts](https://github.com/deepseek-ai/deepseek-harness/blob/c291e7961a515f6d7af9304e7fd1d257929aef26/packages/shell/bash-local/src/index.ts) / tests/executor.spec.ts 在各自本地执行入口按 cwd 启动进程、收集输出，没有此处的目录依赖缓存可移植。Eino `9d983b36a5112a1c233056b1a099825298fafb8f` 的 [backend_inmemory.go](https://github.com/cloudwego/eino/blob/9d983b36a5112a1c233056b1a099825298fafb8f/adk/filesystem/backend_inmemory.go) / TestInMemoryBackend_LsInfo 覆盖根目录和子目录列举，亦不是命令结果缓存。未运行上游测试；Threadmill 保留已有缓存，只补漏掉的根依赖及旧索引隔离。

验证收尾：全仓测试通过；Btrfs 上公开跨 agent listing 回归通过且未跳过。并行跑全仓与 race 时，exec 的未改动用例 TestSchedulerReapWaitsBeforeRemovingRuntimeDir 一次报“清理后目录仍存在”；原用例独立连续 3 次通过，完整 exec race 单独复跑通过。保留首次失败及重跑日志，不声称已经修复其清理时序风险。新构建 threadmill-root-listing-fix 与指纹见 root-listing-build.json，尚未部署。

15:29 文件恢复第二轮 Verifier PASS，Manager 发布 restore-stdin-products。直接只读对照发布目录：六个文件 SHA-256 与恢复前保存的独立期望全部一致，没有额外或缺失文件，见 receipt-preservation-final.json。Verifier 复跑 11 项测试与文件路径/stdin 三种情况；由于五份现有产物字节不变，不再重复执行同一验证。GUI 展示完整六文件交付并进入 Manager 尾部记忆整理，见 receipt-six-file-publish.png。修复成功，但 28m29s 的第二次激活仍包含旧缓存误导后的多余复制，不能算新缓存修复或 v21 候选的收益。

15:32 8789 已 busy=false、pending=0、task/model/memory/tool 活动均零；保存旧二进制后原子更新为 threadmill-root-listing-fix，重启候选服务并由可见 GUI 重新打开 receipt-logsummary。核对实际进程可执行文件 SHA-256 `b5101cb0e66deaf98fd116465b444f824c9e2a8ebfadfc8c191ea59da02b9b33`，11 个已完成任务、零取消、六文件哈希不变，配置逐项等于 v19。8787/8788 均未重启。证据 root-listing-deployment.json、receipt-root-listing-deployed.png。重开后 UI 显示消息为空，这是 ui-agent-api.md 已说明的内存消息历史限制；图与文件仍保留，不把此次空闲重开等同于此前会取消等待者的错误恢复。两个未验证提示候选继续单独隔离。
