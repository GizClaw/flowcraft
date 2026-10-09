# Anvil —— 一个最小的 craft 宿主应用

Anvil 是 `craft` 模块的可运行示例。它是一个很小的宿主应用：用一份
`craft.yaml` 定义、一个编译期 capability 和一个插件目录组装出一个 Craft，
然后把真实外壳（桌面端、HTTP 服务、无头进程）会走的 craft 生命周期走一遍：
扫描插件、启动宿主、按 key 打开 runtime、调用工具、重载 runtime、热插拔插件、
关闭。

它不需要模型 provider、不需要网络、也不需要任何凭证 —— 整趟演示是本地且确定的，
所以它同时也是一份集成测试。

English version: [README.md](README.md).

## 快速开始

前置条件：Go 1.26+。插件的 MCP server 是一个 Go 程序，manifest 用 `go run`
启动它，因此**首次**运行会先编译它（几秒钟，之后走缓存）。

```bash
cd examples/anvil
go run .
```

在仓库根目录运行：

```bash
go run ./examples/anvil -definition examples/anvil/craft.yaml -data examples/anvil/.anvil
```

参数：

- `-definition <path>` —— craft 定义文件（默认 `craft.yaml`）。
- `-data <dir>` —— 可写根目录：插件状态、插件数据、notes 文件以及演示写入的
   per-runtime layer（默认 `.anvil/`，已加入 gitignore）。
- `-timeout <duration>` —— 整趟演示的时限（默认 `3m`）。

每次运行，演示都会先清掉它自己在数据目录里创建的文件，因此可以反复运行；数据目录里
其余内容不受影响。

## 演示内容

| 步骤 | 发生什么 | 对应的 craft 概念 |
| --- | --- | --- |
| definition | 解析并校验 `craft.yaml` | 定义是数据，应用是代码 |
| plugins | 扫描插件目录，列出权限与 skill 根目录 | 插件 store；`skills:provide` |
| start | 启动 Craft，插件的 MCP server 起来后发布工具 | 插件宿主与共享工具集 |
| `runtime "default"` | 打开第一个 runtime（带 values），通过它的 assembly 调工具 | 按 key 的 runtime、`${craft:...}`、工具 assembly |
| `runtime "alpha"` | 用一份 per-runtime layer 打开第二个 runtime | per-runtime layer、runtime 隔离 |
| reload `"alpha"` | 改写 layer 文件后重载 runtime | 事务式重载 |
| hot plug | 禁用再启用插件 | 插件贡献被撤回与恢复 |
| shutdown | 关闭 runtime 与 Craft | 先 drain 再 close，逆序关闭 |

几个关键输出：

```text
== runtime "default"
   tools: hello__greet, hello__ping_host, notes_add, notes_list
   notes_add  "Append one line to the default runtime's notes file."
   event  craft.runtime.opened  {"key":"default"}
   notes_add {"text":"buy milk"} -> added note 1 to /data/notes-default.txt
   notes_list {} -> 1 note in /data/notes-default.txt: buy milk
   hello__greet {"name":"anvil"} -> Hello, anvil! (from the hello plugin)
   event  plugin.hello.ping  {"from":"hello"}
   hello__ping_host {} -> host protocol 1 (host version 0.0.0): 5 primitives exposed, 2 callable by this plugin [emit_event, host_about]; needs a grant for [secret_delete, secret_get, secret_set]; 8 unconfigured (no_service)
```

（上面与下文中的绝对路径都省略成了 `/data/`。）

这一段里发生了三件事：

1. `notes_add` / `notes_list` 是 **capability** 注册的资源类型 `workshop.Notes`
   提供的工具，操作的文件来自该 runtime 的 `${craft:notes}` value。
2. `hello__greet` 是 **插件**贡献的工具：插件自带的 MCP server 声明了 `greet`，
   craft 给它加上了插件 id 前缀。
3. `hello__ping_host` 是插件反过来调用 **host primitives**（通过 host MCP
   端点调用 `host_about` 与 `emit_event`）；它发出的事件以 `plugin.hello.ping`
   出现在 craft 平面上，而 `host_about` 会报告宿主暴露了哪些原语、当前插件能调用
   哪些、其余的为什么够不着（`no_grant`、`no_service`、`disabled`）。

```text
== runtime "alpha"
   layer /data/alpha.layer.json: notes.path -> notes-alpha.txt
   notes_add  "Append one line to the alpha runtime's notes file."
   notes_list {} -> no notes yet in /data/notes-alpha.txt
```

同一份定义、第二个 runtime key：它有自己的工具目录、自己的 notes 文件。default
runtime 的文件来自 runtime **values**，alpha 的来自应用写出的 runtime **layer**，
两者最终都汇入同一份合成文档。

```text
== reload "alpha"
   rewrote the layer: notes.path -> notes-alpha-round2.txt
   event  craft.reload.started  {"key":"alpha","reason":"manual"}
   event  craft.reload.completed  {"key":"alpha","reason":"manual"}
   notes_list {} -> no notes yet in /data/notes-alpha-round2.txt

== hot plug: disable and enable the hello plugin
   event  craft.reload.completed  {"key":"default","reason":"plugin"}
   tools: notes_add, notes_list
   disabled: the plugin's tools are withdrawn from every open runtime
   tools: hello__greet, hello__ping_host, notes_add, notes_list
   hello__greet {"name":"the restarted plugin"} -> Hello, the restarted plugin!
```

重载会重建 deployment generation 并原子替换；而插件变更只触及部署文档与共享工具集，
所以热插拔永远不会撕裂资源注册表。两者都以 `craft.reload.*` 事件出现在应用本来就在
监听的同一个平面上。

## 文件说明

| 路径 | 作用 |
| --- | --- |
| `craft.yaml` | 定义：元信息、插件根目录、`host_tools` 绑定、一份内联 `deploy` 文档 |
| `workshop.go` | 唯一的编译期 capability：`workshop.Notes` 资源类型（工具源）、events 与 secrets 服务实现 |
| `assemble.go` | 宿主装配：路径 → 插件 store → 插件宿主 → `craft.New` → 事件 sink；以及演示用的辅助函数 |
| `tour.go` | 脚本化的生命周期演示 |
| `events.go`、`print.go` | craft 平面 sink 与带互斥锁的打印器 |
| `tour_test.go` | 同一趟演示的测试版本（`go test ./…`） |
| `plugins/hello/plugin.json` | manifest：权限、一个 skill 目录、一个 MCP server |
| `plugins/hello/server/main.go` | 插件的 stdio MCP server（go-sdk）及它的 host primitive 客户端 |
| `plugins/hello/skills/hello/SKILL.md` | 宿主通过 `SkillRoots()` 暴露的 skill |

## 可以照搬到你自己应用里的规则

1. **定义是数据，capability 是代码。** `craft.yaml` 只能引用已注册工厂提供的
   资源类型、内置类型或服务实现 —— 否则 `craft.New` 直接失败，之后注册表就冻结了。
   Anvil 恰好注册三组工厂（`event.Bus`、`tool.Assembly`、`workshop.Notes`）和两个
   具名服务实现（`events`、`secrets`）。
2. **路径由应用决定。** 定义里写的是 `plugins.roots: [plugins]`；`assemble.go`
   相对 `craft.yaml` 所在目录解析它，并自行决定插件状态放在哪（anvil 放在数据目录
   而不是定义旁边）。
3. **values 与 layers 是 per-runtime 的两个旋钮。** values 供 `${craft:...}`
   取值（default runtime 的 notes 文件），layers 按 key 覆盖整棵子树（alpha 的
   notes 文件）。缺 value 会以 `craft: reference ${craft:notes} is not defined`
   构建失败。
4. **插件工具是异步到达的。** `craft.Start` 返回时插件的 MCP server 尚未完成握手，
   所以应用要等就绪（`waitForPluginTool`）再打开需要这些工具的 runtime；已经打开的
   runtime 会通过共享工具集得到后续变化。
5. **等 craft 平面的事件，而不是 sleep。** 重载效果落在 craft 自己的 goroutine 上；
   演示订阅了 `craft.>`（外加插件命名空间），并在触发重载后等
   `craft.reload.completed`。
6. **重载后要重新读取 runtime 的资源。** 重载会换入新一代 generation，
   `rt.Resource("tools")` 返回的是新的 assembly。
7. **宿主关闭 stdin 时，插件进程应以 0 退出。** 这是 MCP stdio 约定的关闭方式；
   非零退出会被插件宿主报告为 stop 失败。`plugins/hello/server/main.go` 里有这段
   判断。

## 插件

`plugins/hello` 是一个完整的插件：manifest、skill 和一个 MCP server。

- `mcp: {command: "go", args: ["./server"]}` —— 带路径的参数的会相对插件根目录
  解析并被限制在其中，所以 manifest 只携带源码、由宿主在启动时编译。真实插件应发布
  编译好的二进制（或解释器 + 脚本，例如 `python3 server/main.py`）。
- `permissions: [mcp:provide, skills:provide, events:emit]` —— 没有对应权限的贡献
  会被 fail-closed 地丢弃，权限集合同时决定宿主发给插件的 bearer token 的范围。
- 宿主向子进程注入 `CRAFT_PLUGIN_ID`、`CRAFT_PLUGIN_DATA_DIR`、
  `CRAFT_HOST_MCP_URL` 和 `CRAFT_PLUGIN_TOKEN`。token 按插件声明的权限签发，所以
  这个插件能调 `emit_event` 而调不了 `secret_get`。
- 示例故意不写 `minHostVersion`：对开发版 craft（`Version == "0.0.0"`）来说，
  任何高于 `0.0.0` 的下限都会让插件被判为无效。等你锁定正式发布的 craft 之后再设置它。

## 发布前说明

`craft` 还没有 release tag，因此 `go.mod` 从同级目录解析它：

```go
replace github.com/GizClaw/flowcraft/craft => ../../craft
```

craft 第一次打 tag 后，去掉这个 `replace`、锁定发布版本，示例就能像
`examples/parallel` 一样以 `GOWORK=off` 独立构建。在那之前请在仓库内运行本示例：
插件的 `go run` 需要 workspace 才能解析 craft。

## 继续扩展

- **加一轮 agent 对话**：把 `flowcraft-config` skill 的
  `assets/minimal-craft` 里的部署资源（workspace、provider、inference assembly、
  graph agent）搬进来，然后在 runtime 上打开 session。
- **加一个 graph node**：在 manifest 里声明 `nodes` 与 `nodes:provide`，并把目标
  agent 写进 `plugins.node_targets`；craft 会为每个 node 合成一个 `graph.NodeType`
  资源并接进 agent 的 engine deps。
- **加一个 capability**：新的资源类型、`${...}` resolver、layer 或 host factory
  decorator —— craft 通过类型断言发现这些可选接口。
- **加第二个插件根目录**：`plugins.roots: [plugins, ~/…/plugins]`，或一个只读的
  `plugins.builtin` 目录供用户根目录遮蔽（shadow）。

完整契约见 [`docs/guides/craft.md`](../../docs/guides/craft.md)，`craft.yaml` 与
`plugin.json` 的速查卡见 [`skills/flowcraft-config`](../../skills/flowcraft-config/)。
