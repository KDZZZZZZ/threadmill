# Threadmill WebUI

React 19 + TypeScript 6 + Vite 8 + HeroUI v3 + Tailwind v4。依照指定的 `FRONTEND-STACK.md` 选择 A 级三层结构，保留已有界面的视觉与交互。具体依赖锁定在 `package-lock.json`。

## 开发

需要 Node.js 24。仓库根目录：

```sh
npm --prefix web ci
go run ./cmd/threadmill -web
```

另一个终端启动前端：

```sh
npm --prefix web run dev
```

Vite 默认在本地 5173 端口提供界面，将 `/api/v1` 与 `/healthz` 转发到本地 8787 网关。用 `DEV_API_ORIGIN` 指定其他后端地址。凭据仍由 Go 运行时读取，不进入前端环境变量或构建产物。Threadmill 网关没有登录接口，不引入示例规范中的登录页面或 token store。

## 结构与边界

```text
src/main.tsx                  Provider 链
src/app/router.tsx            路由唯一源
src/pages/workspace-page.tsx  项目页与表单、本地草稿、面板状态
src/components/               对话、Swarm、原有 SVG 图标等展示组件
src/hooks/                    图拖拽缩放、面板尺寸等纯 UI 逻辑
src/lib/types.ts              对齐 docs/openapi.yaml 的类型
src/lib/http.ts               唯一 HTTP / SSE 入口
src/lib/api/projects.ts       项目资源的 URL、query key、查询和 mutation hooks
src/lib/project-events.ts     不可变事件投影、消息去重、Help 恢复
src/stores/theme-store.ts     zustand 本地主题偏好
src/index.css                 原有视觉样式及深浅主题变量
```

页面调用资源 hooks，展示组件不取数；服务端快照和事件投影留在 Query 缓存，项目草稿等短期本地交互留在页面，zustand 只保存主题。SSE 在切换项目或卸载时关闭；使用十进制字符串序号去重，保留 opaque Agent ID。运行时不可用时显示错误，只有显式 `?demo=1` 才加载示例数据。

保留：项目搜索与折叠、每项目草稿、发送重试的同一回执键、IME 输入、Markdown 清理、推理与报告展开、当前进度的消息顺序、任务生命周期筛选、Help 等待恢复、活动 hover/focus/pin、图平移与缩放、鼠标与键盘分栏、窄屏侧栏。默认深色样式沿用原设计，新增浅色主题并遵循系统减少动效设置。

## 构建与交付

```sh
npm --prefix web run lint
npm --prefix web test
npm --prefix web run build
go build -o threadmill ./cmd/threadmill
./threadmill -web
```

Vite 输出到 `internal/cli/webassets`；Go `embed` 将它与程序一起发布。生成资源纳入版本控制，`go install` 和一键安装不要求终端用户安装 Node 或单独下载 HTML。修改前端必须同时更新生成资源，CI 会重新构建并检查一致性。

`docs/webui-demo.html` 和旧 Python 检查生成器保留为迁移前的视觉、行为基准，不参与新前端构建。不要继续在旧文件实现新功能。

## 浏览器回归

```sh
node web/node_modules/playwright-core/cli.js install chromium
npm --prefix web run preview -- --port 4173
# 另一个终端
WEBUI_URL=http://127.0.0.1:4173 npm --prefix web run e2e
```

默认测试地址为 `http://127.0.0.1:5174`，可以用 `WEBUI_URL` 覆盖。`CHROME_PATH` 可指定已有 Chromium 可执行文件。脚本覆盖旧消息顺序 6 项、Help 恢复 7 项，以及发送重试、项目切换、断线、Markdown 安全、主题、减少动效、分栏、图操作和移动布局；深浅截图分别输出到 `e2e/shots-dark`、`e2e/shots`。HTTP/SSE 使用测试数据，不触发收费模型调用；Go 网关连接和资源交付另由 `internal/cli` 测试验证。

直接验证 Go 网关、真实 Manager 测试 provider、内置 React 与 SSE（无外部模型调用）：

```sh
CHROME_PATH=/path/to/chromium THREADMILL_BROWSER_E2E=1 \
  TMPDIR=/short/path/to/reflink-tmp go test ./internal/cli \
  -run '^TestWebStreamsManagerMessagesAndDeduplicatesSubmissions$' -count=1 -v
```

浏览器临时目录路径应保持较短，避免超过 Unix socket 路径上限。

重构前已运行 `go test ./...`（Btrfs TMPDIR）通过，原浏览器消息顺序 6/6、Help 7/7、Markdown 检查全部通过。视觉基准来自同尺寸的原 WebUI 截图；不能用演示截图证明模型效果或评测分数。

## 文档依据与参考

- [React Effect 订阅与清理](https://react.dev/reference/react/useEffect#connecting-to-an-external-system)、[TanStack Query 缓存更新](https://tanstack.com/query/latest/docs/framework/react/reference/QueryClient#queryclientsetquerydata)。
- [HeroUI v3 安装](https://heroui.com/docs/react/getting-started/quick-start.mdx)、[Button](https://heroui.com/docs/react/components/button.mdx)、[Tailwind Vite 插件](https://tailwindcss.com/docs/installation/using-vite)。HeroUI 本地文档缓存位于 `.heroui-docs/react` 与 `.heroui-AGENTS.md`，可用 `npm run docs:heroui` 重建。
- [React Router data router](https://reactrouter.com/start/data/installation)、[Vite 构建与许可证输出](https://vite.dev/config/build-options.html#build-license)、[Go embed](https://pkg.go.dev/embed)。
- 横向检查 deepseek-harness `c291e796` 的 [`apps/web/src/main.ts`](https://github.com/deepseek-ai/deepseek-harness/blob/c291e7961a515f6d7af9304e7fd1d257929aef26/apps/web/src/main.ts) 和 [`chat-scroll-contract.e2e.ts`](https://github.com/deepseek-ai/deepseek-harness/blob/c291e7961a515f6d7af9304e7fd1d257929aef26/apps/web/tests/chat-scroll-contract.e2e.ts)：入口与应用分离，浏览器验证消息几何与滚动归属。本次沿用 Threadmill 的 API 与原布局，不引入其应用框架。
- Pi `ceea48f5` 当前代码树未找到 WebUI，原 `packages/web-ui/package.json` 路径返回 404；Eino `9d983b36` 的代码树与 README 为 Go 编排框架，没有对应前端。没有声称复用了不存在的实现。

构建产物中的 `assets/licenses.txt` 包含依赖许可证，`assets/design-license.txt` 保留原 Beautiful UI 视觉设计署名。原 Markdown 库与来源见 [第三方说明](../docs/webui-third-party.md)。
