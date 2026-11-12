# Forge — 可运行的本地工作区示例

Forge 是 FlowCraft 的可运行本地示例：把一个工作区目录变成部署好的 agent，用终端
界面或脚本测试驱动一次会话，并把所有场景事实留在 `scenarios/` 下的纯文件里。

它是照着当前 core 的分层重写的，因此也是一个"最小而完整"的宿主：应用真正要写的
代码就这么多。

English version: [README.md](README.md)。

## 快速开始

前置条件：Go 1.26+ 和一个 OpenAI 凭据。

```bash
cd examples/forge
cp .env.example .env         # 填入 OPENAI_API_KEY=...
go run . help
go run . tui new             # 选 companion 直接开聊
go run . test -test companion/opening
```

## 命令

| 命令 | 作用 |
|---|---|
| `forge workspace create --config <raid> --workspace <dir>` | 把 raid 场景复制成工作区 |
| `forge workspace inspect --workspace <dir>` | 打印 agent、引擎和缺失的环境变量 |
| `forge config raid\|persona\|test list` | 列出可用场景和测试 |
| `forge tui new` / `forge tui resume` | 打开交互 TUI（新建或恢复工作区） |
| `forge test -test <raid>/<name> [--timeout 2m]` | 跑一条脚本测试，产物写到 `.out/<run>/` |
| `forge test-auto --raid <raid> --persona <persona> [--turns 3]` | 让两个已部署 agent 互相对话 |

场景按优先级从 `--scenarios`、`FORGE_SCENARIOS`、可执行文件目录、当前目录、
用户配置目录依次解析。

## 一个工作区的结构

```
scenarios/raids/companion/
  deploy.yaml            # 有哪些 agent，每个怎么构建
  runtime.yaml           # 底座：边界、文件根、模型、工具
  graphs/assistant.yaml  # 一张图：state、nodes、edges
scenarios/personas/tom/  # 同样三个文件，一个 agent，无工具
scenarios/tests/companion/opening.yaml
```

`deploy.yaml` 只声明 agent，不描述资源图：没有 resource、没有装配步骤、文档里也
没有依赖注入。

```yaml
version: 1
agents:
  - name: assistant
    engine:
      kind: graph
      settings:
        path: ${workspace:root}/graphs/assistant.yaml
    runtime:
      layers:
        - priority: 0
          path: ${workspace:root}/runtime.yaml
    catalog: {scope: session}
```

`${workspace:root}` 是 forge 为"当前打开目录"注册的占位符 scheme
（`internal/app/document.go`），所以场景目录可以整体搬走而不用改文件。

`runtime.yaml` 就是一个 `runtime.Config`：沙箱后端、可写目录、模型（driver 自带
模型清单，模型由部署声明）以及启用哪些内置工具集。模拟设备工具不在这里——它是
宿主挂载的一个 `tool.Source`（`internal/simtools`），应用加自己的工具就走这条路。

`graphs/assistant.yaml` 才是 agent 本身：带合并规则的 state 键、节点，以及条件为
state 表达式的边。

```yaml
state:
  messages: {type: messages, merge: append}
  pending:  {type: json}
nodes:
  - {id: model, kind: model, config: {messages: messages, pending: pending, output: answer}}
  - {id: tools, kind: tool,  config: {messages: messages, pending: pending}}
edges:
  - {from: model, to: tools, when: len(state.pending) > 0}
  - {from: tools, to: model}
  - {from: model, to: end}
output: {key: answer}
```

注意 `len(state.pending) > 0` 而不是 `!= nil`：model 节点总会写这个列表，空列表
也不是 nil，用 `!= nil` 会死循环。

## 代码在哪

| 路径 | 职责 |
|---|---|
| `internal/app/app.go` | 打开工作区：manager、store、session、run 迭代器 |
| `internal/app/document.go` | 读 `deploy.yaml`、解析 `${workspace:root}`、报告缺失环境变量 |
| `internal/app/drivers.go` | 本二进制编入的 driver 表（`openai`、`anthropic`） |
| `internal/app/usage.go` | 把一轮的 usage 事件折成一份报告 |
| `internal/simtools` | 示例自己的工具，作为 `tool.Source` |
| `internal/chatfmt` | 事件流 → 语音/工具块，TUI 与测试日志共用 |
| `internal/tui` | bubbletea 界面 |
| `internal/commands` | CLI 表面 |
| `internal/scenario` | 文件解析与复制，与 agent 无关 |

整个宿主就是 `host.New` + `Loader.Load` + `Manager.Deploy`：

```go
mgr := host.New(deploy.BuiltinKinds(), host.Bindings{
    Store:          store,                                     // 恢复会话
    RuntimeOptions: []runtime.Option{drivers, simtools.New()}, // 接线
    LoadOptions:    workspaceOptions(dir),                     // ${workspace:root}
})
specs, err := mgr.Loader().Load(ctx, body)
_, err = mgr.Deploy(ctx, specs...)

session, _ := mgr.Session("assistant", app.SessionID)
for event, err := range session.Run(ctx, message.NewTextMessage(message.RoleUser, text)) { ... }
result, err := session.Result(ctx)
```

## 与旧版 forge 的差异

旧版写在 `core` v0.10.0 上，已经无法在当前分层下编译。这次重写跟着新分层走：

- **没有资源文档**：`resources:` / `deps:` / `kind: …impl: …` 全部消失，
  一次部署就是 agent + 引擎设置 + 运行时层。
- **没有 JS 脚本运行时**：图的内置节点类型只有 `model`、`tool`、`condition`。
  旧的 `werewolf`、`multi_role_storyteller` 由约 2300 行 JavaScript 驱动，
  现在只保留在 `../forge-tmp` 作参考——core 里已经没有任何脚本引擎。
- **没有 board**：图 state 取代 board 变量，条件读 `state.<key>`。因此单轮无法
  覆盖模型：model 节点的请求是配置，TUI 的 `/model`、`/think` 已移除，模型在
  `runtime.yaml` 里定。
- **没有逐节点说话人标签**：事件只说明模型说了什么、哪个工具跑了，不说明是哪个
  节点产生的。需要署名的部署给每个说话人一个 agent（一个会话一个说话人），
  按 agent 名标注。
- **工具计数与 usage 来自事件流**，不再用包装 host factory 的装饰器。
- **会话可持久化**：每个工作区在 `.forge/sessions.db` 有自己的 SQLite store，
  所以 `tui resume` 恢复的是对话，不只是目录。

## 开发说明

本示例需要的 core 修订还没有打 tag：core 当前是 `v0.10.0`，早于
`core/agent-host` 和 `core/agent-graph/nodes`。因此这个模块通过仓库根目录的
`go.work` 解析依赖，自身不带任何 `replace`。等下一个 core 版本发布后把 require
提升到新版本，`make tidy` 和 `make check-consumers`（用 `GOWORK=off` 按发布版本
构建）才会重新变绿。

校验，在本目录下（workspace 模式）：

```bash
gofmt -l .
go build ./... && go vet ./... && go test ./...
```

以及仓库根目录的 `make ci`。
