# Benchmarks

这里存放 Threadmill 的外部 benchmark 适配、固定版本和运行说明。上游题库、
容器层、模型密钥和运行结果不进入 Git；它们统一落在 `.work/`，也可以用
`THREADMILL_BENCHMARK_HOME` 指向更大的独立磁盘。

每个 benchmark 目录至少回答四件事：

1. 官方来源和固定版本是什么；
2. 如何检查本机环境与题库完整性；
3. 如何启动官方评分链路；
4. 哪些结果可以横向比较，哪些只能算 Threadmill 实验结果。

当前接入：

- [`swe-refactor-bench/`](swe-refactor-bench/README.md)：20 道整仓技术栈迁移题，
  Harbor 驱动，三阶段隐藏验收。
- [`deep-swe/`](deep-swe/bench)：固定 113 道原创新增长周期任务，按上游要求由
  Pier 驱动。
- [`senior-swe-bench/`](senior-swe-bench/bench)：固定 Senior SWE-Bench
  v2026.06 公共 Harbor 数据集。
- [`swe-marathon/`](swe-marathon/bench)：固定超长周期整仓任务集。

四套入口共享 [`harbor/threadmill_agent.py`](harbor/threadmill_agent.py) 这一层薄
适配：上传当前二进制、传入任务指令、收集 Threadmill 轨迹和恢复状态；题目、
容器和 verifier 仍由固定上游负责。DeepSWE 使用其官方 Pier 分支，其余入口使用
Harbor。适配后的分数是相应官方 grader 上的 Threadmill 实验结果，不冒充上游
固定 harness 的排行榜成绩。

Threadmill agent 使用项目相对路径，不创建 Git 提交。VFS 保留软链接及
`.gitignore` 匹配的工作文件，输入安装不覆盖 `.git` 元数据。
Manager 创建 `real_directory=true` 的新 task 完成交付：真实目录现有内容作为额外来源
先进入 pending 合入，准备完成后该 task 直接在真实目录运行和调试。其他 task
继续使用隔离工作区；失败时真实改动也已可见，须以 verifier 结果判断验收。现有 VFS 内存增量限制仍为
单文件 50 MiB、一次吸收的修改内容合计 200 MiB；大构建产物可能触发这些限制，
不能据适配测试通过就认定完整模型评测全部可运行。

DeepSWE 使用 [`harbor/threadmill_pier_agent.py`](harbor/threadmill_pier_agent.py)
对接 Pier 的 Agent 接口，在 `/app` 运行。已验证 Pier 0.3.1；模型连接读取
`<PROVIDER>_BASE_URL` / `<PROVIDER>_API_KEY`，或通用的 `OPENAI_BASE_URL`
（也接受 `OPENAI_API_BASE`）/ `OPENAI_API_KEY`。

DeepSWE 每次启动将固定题库复制到 `.work/deep-swe/run.*/`：只调整提交物采集为
通过临时 Git index 导出工作区差异（含新增文件、删除和软链接），并通过 Pier 环境适配层
添加 VFS 挂载权限。环境适配层保留 Pier 的模型域名出口代理和任务网络限制。原始题库、基线提交、测试及评分脚本保持原样。
临时 index 不修改项目的 HEAD 或原 index。运行目录保留供结果追溯；这项采集
差异应随 Threadmill 实验成绩一同说明。

评测环境也必须满足强制 reflink 准入：项目、VFS 存储和需要克隆的 Gradle／缓存产物必须支持实际 CoW 克隆。只有 OverlayFS 或 Docker 权限不够；禁止用普通复制维持兼容。请在支持 reflink 的测试卷上准备这些路径。适配器单元测试通过不代表任意评测镜像均满足这一系统条件。
