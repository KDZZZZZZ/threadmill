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
