# Threadmill 权限与容器运行模式

本文描述 Threadmill 在**完整能力、不降级**条件下所要求的权限，以及如何把这些能力放进 Docker。Docker 只改变 Threadmill 所处的外层边界，不改变 VFS、reflink、bwrap、任务调度或 Agent 工作区的内部实现。

## 1. 完整能力的权限模型

Threadmill 有两层隔离：

```text
外层项目容器（可选）
└── Threadmill 进程
    ├── VFS：项目 floor、live workspace、reflink 快照
    ├── bwrap：每条 Agent 命令的用户、PID、mount namespace
    └── Agent 命令：只能看到当前任务的工作区
```

完整能力必须同时满足以下条件：

| 能力 | 用途 | 失败时的行为 |
| --- | --- | --- |
| 项目目录读写 | 读取基线、写入发布结果 | 项目不能打开 |
| VFS 存储目录读写 | 保存 floor、live workspace、事件输入和任务状态 | 项目不能打开 |
| 实际 reflink/CoW | 快速创建隔离工作区，禁止普通复制替代 | 项目不能打开 |
| bwrap | 为 Agent 命令创建内部沙箱 | 安装或启动前失败 |
| user/PID/mount namespace | 隔离用户、进程和文件系统视图 | bwrap 探测失败 |
| `/usr`、`/bin`、运行库和证书只读可见 | 让命令和工具链可运行 | 命令沙箱不可用 |
| 临时目录读写 | `TMPDIR`、编译缓存、命令输出和追踪文件 | 命令失败 |
| 受控网络出口 | 模型请求、包下载或项目测试 | 按网络策略失败，不回退到宿主网络 |
| 资源限制 | 并发、超时、输出、进程和内存上限 | 任务被终止 |

Threadmill 不允许把上述失败降级成宿主执行或普通复制。安装器和项目启动流程应在真正使用前完成探测。

## 2. 宿主机直接运行

直接运行时，安装器需要确认：

- 当前用户可以执行 `bwrap`；
- bwrap 能创建 user/PID/mount namespace；
- 项目目录和 VFS 目录支持实际 `FICLONE`；
- 当前用户可以读写这两个目录；
- 所需运行库、证书、shell、编译器和追踪工具可见；
- 模型凭据只通过 Threadmill 的凭据文件或受控环境变量提供。

此模式不需要 Docker daemon，也不需要把宿主根目录交给 Threadmill。bwrap 只读挂载运行时需要的系统目录，并把任务 live workspace 映射到 `/workspace`。

## 3. Docker 可以提供什么

Docker 适合提供**项目级外层边界**：

- 项目专属的根文件系统、工具链和依赖版本；
- 项目级网络、CPU、内存、PID 数量和生命周期限制；
- 项目目录和 VFS 目录的显式挂载；
- 评测任务结束后的整体销毁；
- 日常开发和评测使用同一 Threadmill 二进制与配置接口。

Docker 不会自动提供以下能力：

- reflink；
- 内层 bwrap 所需的 namespace 权限；
- 对任意宿主路径的访问；
- 模型网络出口；
- 跨挂载点的 CoW 克隆。

Docker bind mount 只是把宿主路径映射到容器。`FICLONE` 要求源文件和目标文件位于同一支持 reflink 的文件系统，因此项目目录和 VFS 目录必须显式放在同一宿主卷中。默认 `overlay2` 容器层或独立 Docker volume 不能直接视为满足条件。

## 4. 运行完整 Threadmill 镜像所需的 Docker 权限

镜像内部仍然运行 bwrap，因此外层容器需要允许内部 namespace 操作。生产配置应按最小可用集合配置，而不是直接依赖默认 Docker：

```sh
docker run --rm -it \
  --cap-add=SYS_ADMIN \
  --security-opt seccomp=/path/to/threadmill-seccomp.json \
  --security-opt apparmor=threadmill-bwrap \
  --mount type=bind,src=/srv/threadmill/projects,dst=/workspace \
  --mount type=bind,src=/srv/threadmill/vfs,dst=/threadmill-vfs \
  threadmill:latest
```

实际启动器必须在进入任务前探测：

1. `bwrap` 是否存在；
2. user/PID/mount namespace 是否可创建；
3. `mount`、`unshare`、`clone` 是否被 seccomp 或 AppArmor 拒绝；
4. `/workspace` 和 `/threadmill-vfs` 是否为同一支持 reflink 的文件系统；
5. `cp --reflink=always` 或等价 `FICLONE` 是否成功；
6. 容器内用户是否拥有项目和 VFS 的读写权限。

探测失败必须阻止项目启动。不能通过 `--privileged` 作为默认修复，也不能退回到普通复制或宿主命令执行。若某个发行版需要更宽的 Docker 安全配置，应把它写进该发行版的镜像运行说明，并继续保留启动探测。

## 5. reflink 的 Docker 布局

推荐的宿主目录布局：

```text
/srv/threadmill/                         # 同一支持 CoW 的文件系统
├── projects/<project-id>/                # 评测或日常项目基线
└── vfs/<project-id>/                     # Threadmill live/floor 存储
```

容器内对应为：

```text
/workspace       -> /srv/threadmill/projects/<project-id>
/threadmill-vfs  -> /srv/threadmill/vfs/<project-id>
```

不要把项目 bind mount 到 Btrfs，而把 VFS 放进 Docker 默认 overlay2；也不要把两者放到不同宿主挂载点。Docker Desktop、远程 Docker daemon、NFS 和 FUSE 文件系统都必须在实际目标环境中重新做 reflink 探测。

## 6. 评测容器与日常 Docker 的统一接口

评测和日常使用应共享同一内部 Threadmill 实现：

```text
Threadmill 核心
├── VFS / reflink
├── bwrap executor
├── scheduler / resource limits
└── project API
        ↑
        ├── DirectHostLauncher
        ├── DailyDockerLauncher
        └── BenchmarkDockerLauncher
```

外层启动器只负责：

- 准备镜像和工具链；
- 挂载项目与 VFS；
- 配置模型出口和评测网络；
- 注入资源限制；
- 采集日志和结果。

它不应替换 Threadmill 的执行器，也不应自行实现文件复制、任务隔离或结果合并。这样可以在不改 Agent 行为的情况下切换：

- 本机直接运行；
- 日常开发 Docker；
- Harbor/Pier 等评测容器。

三种模式只改变配置来源和外层边界。Threadmill 内部仍然使用相同的相对工作区路径、VFS 事件、bwrap 命令执行和 reflink 准入。

## 7. 推荐配置原则

- 默认启用完整检查，不提供静默降级开关；
- 启动日志记录文件系统设备号、文件系统类型、reflink 探测结果、namespace 探测结果和 bwrap 版本；
- 日常 Docker 与评测 Docker 使用同一镜像入口和同一健康检查；
- 评测只额外改变网络代理、资源上限和日志挂载；
- 外层容器销毁不影响结果保留目录；
- 所有宿主路径都由启动器显式传入，不根据容器内绝对路径猜测宿主位置。

相关实现：

- [命令 bwrap 执行器](../internal/exec/bwrap.go)
- [Docker 执行后端](../internal/exec/docker.go)
- [VFS 持久化存储](../internal/vfs/store.go)
- [Harbor 评测适配器](../benchmarks/harbor/threadmill_agent.py)
- [用户配置说明](configuration.md)
