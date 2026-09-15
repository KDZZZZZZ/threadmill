# 外层容器部署

Docker 只包住一个项目，Threadmill 进程及其每条 Agent 命令仍使用 bwrap。评测容器和日常容器调用同一个镜像入口；只改变挂载、网络和资源参数，不替换内部执行器，也不允许复制降级。

## 构建

在仓库根目录先构建二进制，再构建镜像：

```sh
go build -o threadmill ./cmd/threadmill
docker build -t threadmill:local .
rm threadmill
```

## 运行

把项目和状态放在同一宿主 CoW 文件系统下（例如同一 Btrfs 子卷）：

```text
/srv/threadmill/
├── project/       # 项目工作区
├── state/         # HOME（~/.threadmill：配置、VFS、事件）
└── config.yaml
```

启动前会执行 `threadmill -check`，检查 bwrap、namespace、目录权限和 reflink；任何失败都会退出，不会改用普通复制或宿主执行。

```sh
THREADMILL_DATA=/srv/threadmill docker compose -f deploy/docker-compose.yml up
```

若内核禁止容器内创建 namespace，需由管理员提供允许 bwrap 的 seccomp/AppArmor 配置；不要用 `--privileged` 作为默认修复。Docker 的默认 seccomp 会拦截部分 namespace、mount 系统调用，参见 <https://docs.docker.com/engine/security/seccomp/>。

## 评测与日常使用

二者均使用此镜像和入口。评测启动器可以额外设置 `--network none`、CPU/内存/PID 限制和只读工具链；日常使用可以发布端口并挂载模型凭据。`THREADMILL_ROOT`、配置格式和 bwrap 执行路径保持不变。

`/data` 必须是一个宿主挂载点；不要把项目 bind mount 到一个文件系统、状态目录放进 Docker `overlay2` 层，否则 reflink 可能跨文件系统而被启动检查拒绝。
