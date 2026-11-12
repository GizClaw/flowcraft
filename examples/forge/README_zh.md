# Forge —— 跑在 FlowCraft 上的一桌狼人杀

Forge 是 FlowCraft 的可运行本地示例。它把一个 workspace 目录部署成一小群 agent，然后
打一局狼人杀：一个裁判持有规则，八个座位发言，其中一个座位由你本人来打。

它是一个 **host**，不是框架：它和这套栈打交道的每一步，都是任何应用都要自己写的那几步
—— 读工作区、部署 agent、驱动一个会话、再把事件读出来。

English version: [README.md](README.md).

## 快速开始

需要 Go 1.26+ 和 OpenAI 凭证。

```bash
cd examples/forge
cp .env.example .env          # 然后写入 OPENAI_API_KEY=...
go run . help
go run . tui new              # 选 "werewolf"，开局
go run . test -test werewolf/opening
go run . test-auto --raid werewolf --rounds 8     # 让它自己打一局
```

## 命令

| 命令 | 作用 |
|---|---|
| `forge workspace create --config <raid> --workspace <dir>` | 把副本复制成一个工作区 |
| `forge workspace inspect --workspace <dir>` | 列出每个 agent：导演还是发言者、名字、图、模型、工具、委派目标、钩子 |
| `forge inspect <workspace> [--task <id>] [--kind <kind>]` | 从 store 里把一个 run 折叠成时间线 |
| `forge config raid\|persona\|test list` | 列出场景与测试 |
| `forge tui new` / `forge tui resume` | 交互式牌桌 |
| `forge test -test <raid>/<name> [--timeout 2m]` | 跑一条脚本化测试，产物写到 `.out/<run>/` |
| `forge test-auto --raid <raid> [--player <agent>] [--rounds 24]` | 自动打完整局，人类座位由它自己的 agent 回答 |

场景解析顺序：`--scenarios` → `FORGE_SCENARIOS` → 可执行文件目录 → 当前目录 →
用户配置目录。

## 牌桌：一张图，八个 agent

一个副本是一份 **C3** 部署：只有三类参与者。

```text
referee      持有规则和牌桌状态；自己不做任何模型调用
seat_1..8    一人一个 agent，通过 delegate 工具被叫到
你           seat_3，由真人（或 test-auto 里 seat_3 自己的 agent）回答
```

`deploy.yaml` 就是这份演员表：

```yaml
agents:
  - name: referee
    engine: {kind: graph, settings: {path: ${workspace:root}/graphs/referee.yaml}}
    delegation:                       # 同步委派：规则叫一个座位，然后等它回答
      targets: [seat_1, seat_2, seat_3, seat_4, seat_5, seat_6, seat_7, seat_8]
      modes: [sync]
      timeout: 2m
  - name: seat_1
    engine: {kind: graph, settings: {path: ${workspace:root}/graphs/seat_1.yaml}}
    hooks:
      - kind: seat-memory             # 座位跨回合的记忆
        settings: {dir: .forge/memory, keep: 12}
```

裁判的图就是这局游戏：十一个脚本节点（`seed`、`input`、`route`、`night`、`dawn`、
`day`、`vote`、`exile`、`hunter`、`postgame`、`perform`），状态里装着整张桌子。
其中只有 `perform` 真正"动手"—— 它调用 `delegate` —— 于是**图决定叫谁，规则决定问什么**。

```text
input → route → night|dawn|day|vote|exile|hunter|postgame → perform → route
                    ↑                                            │
                    └──────────── 牌桌等你时走向 end ──────────────┘
```

**为什么座位需要钩子。** 委派每次调用都给座位开一个新的子会话；没有属于它自己的事实，
它每晚都是初见这张桌子。所以 `seat-memory` 读写 `.forge/memory` 下属于它的文件，键是
brief 第一行里的对局 id。只有引擎知道一回合何时开始、何时结束；只有部署知道一个座位
能记住什么 —— 钩子类型存在的意义就是这个分工。

## 规则即脚本

一条规则是一段 Starlark，由 `core/agent-graph/script` 在进程内执行：

```yaml
- id: perform
  kind: script
  config:
    backend: starlark
    path: rules/perform.star
    reads: [game, brief]
    writes: [game, brief, spoken]
    allow: [tool:delegate]        # 唯一一条会动手的规则
```

- **声明即全部**：`reads`/`writes` 就是这条规则的接触面，`allow` 是它能用的全部工具。
  文档没写的键和工具，规则碰不到。
- **不能挂起**：脚本一口气跑完。`allow` 非空时禁止在其中挂起，所以"会动手的规则"必然是
  "会跑完的规则"。
- **`version: pinned`**：恢复的 task 继续用当初记录的那一版；那一版不在了就**报错停下**，
  而不是悄悄换规则。
- 终止性是构造出来的：没有 IO、没有时钟，只有步数和输出大小预算。内存不是目标 ——
  选这门语言是为了确定性，不是为了防恶意代码。

后端是语言中立的：先发 Starlark，因为它确定、无 IO；Lua worker 池是**兄弟**，不是替代。

## 钩子：host 拥有的三个时刻

在 `deploy.yaml` 里按 agent 声明：

| 钩子 | 时机 | 失败后果 |
|---|---|---|
| `OnSession` | 会话打开或恢复之后、回合之前 | 终止本回合 |
| `BeforeTurn` | 引擎拿到回合之前，可改写消息 | 终止本回合 |
| `AfterTurn` | run 结束之后，带状态与结果 | **只记日志，不算失败** |

`AfterTurn` 是那个"不能失败"的：活已经干完，副作用无法撤销，所以一次记忆写入输给竞态
应该是一行日志，而不是一局游戏卡住。

## `carry`：跨回合的有界状态

图的 state 键可以声明 `carry: true`，下一个 task 从上一个 task 的状态继续，而不是从零值
开始。裁判用它在任务之间保住牌桌；`day.star` 则靠 `speech` 通道是否变长来判断"人回答了"。

它只适合**有界状态** —— 一局游戏、一张表单、一个累计值 —— 不适合对话历史。在 M2 Max 上
实测（`core/agent-graph/carry_bench_test.go`）：全新状态起一个 task 约 14.7µs；带 200 条
消息的对话每回合约 667µs、约 5k 次分配。**对话属于一个 task，不属于 carry。**

## 读一次 run

`forge inspect` 把 checkpoint store 折叠成时间线 —— 这就是"座位集体弃票"和"座位说的是
我投六号、而解析器读到了字节"的区别：

```text
step 4  perform  (live)
  script.run       rules/perform.star  v=sha256:aa471…  live
  script.tool      delegate#0 ok  我想先救1号，毒药留一晚。  args=sha256:e1dba49…
  script.tool      delegate#1 ok  我这票先弃了。  args=sha256:89c7b63…
  state game     brief_kind= brief_seat=0 current_votes=3 items vote_index=3
```

- 每步一条标记：跑了哪些节点，以及这一步是**真跑还是重放**；
- 只打印这一步**改过**的 state 字段，而不是整份文档；
- 每条规则带版本，每次工具调用带回答与参数哈希。

`delegate#0` 与 `delegate#1` 就是"原始回答"和"重问"：规则读不出票时会再问一次，仍读不出
才记为弃票。

## 代码在哪

| 路径 | 职责 |
|---|---|
| `internal/app` | 打开工作区：manager、store、会话、一个回合 |
| `internal/app/document.go` | 读 `deploy.yaml`、`${workspace:root}`、缺失的环境变量 |
| `internal/app/drivers.go` | 这个二进制编进来的 driver 表 |
| `internal/hooks/memory` | `seat-memory` 钩子类型 |
| `internal/story` | 这张桌子是谁：导演、发言者、名字、玩家座位 |
| `internal/simtools` | 场景自带的工具，作为一个 `tool.Source` |
| `internal/chatfmt` | 事件 → 发言/工具块，TUI 与测试日志共用 |
| `internal/tui` | bubbletea 界面 |
| `internal/commands` | CLI 表面，含 `inspect` |
| `internal/scenario` | 文件解析与复制，与 agent 无关 |

整个 host 就是 `host.New` + `Loader.Load` + `Deploy`：

```go
mgr := host.New(graphKinds(abs), host.Bindings{
    Store:       store,                                       // 恢复
    LoadOptions: workspaceOptions(abs),                       // ${workspace:root}
    OnEvent:     a.forwardSeat,                               // 座位的事件流
})
specs, err := mgr.Loader().Load(ctx, body)
_, err = mgr.Deploy(ctx, specs...)

session, _ := mgr.Session("referee", "referee/"+app.SessionID)   // a session belongs to one agent
for event, err := range session.Run(ctx, message.NewTextMessage(message.RoleUser, text)) { ... }
```

## 工作区结构

```text
scenarios/raids/werewolf/
  deploy.yaml              # 九个 agent：裁判 + 八个座位
  runtime.yaml             # 基座：沙箱、文件、模型、工具
  story.yaml               # 导演、发言者、名字、玩家座位
  graphs/referee.yaml      # 这局游戏：state、十一个节点、边
  graphs/seat_1..8.yaml    # 每个座位一张人格图
  rules/*.star             # 十一条规则
scenarios/tests/werewolf/opening.yaml, full_game.yaml
```

`${workspace:root}` 是 forge 为"正在打开的这个目录"绑定的占位符，所以场景目录可以整体搬家。
每个工作区的持久侧在 `.forge/sessions.db`，因此 `tui resume` 恢复的是**对话**，不只是目录。

## 与旧版 forge 的差别

旧版写于 `core` v0.10.0，已经编译不过。这一版跟随当前分层：

- **没有 resource 文档**：一份部署 = agent + 引擎设置 + runtime 层，文档里没有装配步骤。
- **脚本回来了，但不是 JavaScript**：狼人杀规则是 Starlark，由图引擎自带的 script 节点在
  进程内执行。旧的 JS 运行时保持退役，旧场景留在 `../forge-tmp` 仅作参考。
- **没有 board**：图的 state 取代 board 变量，条件读 `state.<key>`；模型在 `runtime.yaml`
  里选，不再是 TUI 命令。
- **归属靠 agent**：事件只说"说了什么、调了什么工具"，不说"哪个节点产生的"，所以需要按
  发言者归属的部署，就给每个发言者一个 agent —— 座位正是这么来的。
- **用量与工具计数来自事件流。**

## 开发说明

这个示例需要的 core 修订还没有打 tag，所以模块通过仓库的 `go.work` 解析，自己不携带
`replace`。

本目录的门禁：`gofmt -l .`、`go build ./... && go vet ./... && go test ./...`。
仓库根目录：`make ci`（CI 门禁模块另跑 `make lint`）。
