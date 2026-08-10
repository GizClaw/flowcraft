# Forge — 可运行的本地工作区 Demo

Forge 是 FlowCraft 技术栈的可运行本地 demo。它从原生部署文档装配完整
runtime,提供交互式 TUI、脚本化场景测试,以及 raid × persona 双智能体模拟——
全部由 `scenarios/` 下的纯文件驱动,除了 demo 自身外不需要任何应用代码。

English version: [README.md](README.md).

## 快速开始

前置要求:Go 1.26+ 和 provider 凭证(见[凭证](#凭证))。

```bash
cd examples/forge
go run . help
```

运行一条脚本化测试:

```bash
go run . test -test werewolf/opening_setup
```

创建工作区并查看信息:

```bash
go run . workspace create --config werewolf --workspace ./workspace
go run . workspace inspect --workspace ./workspace
```

## 命令

- `forge workspace create --config <raid> --workspace <dir>` — 把 raid 场景复制成工作区。
- `forge workspace inspect --workspace <dir>` — 打印工作区元信息(agent、模型、记忆设置)。
- `forge config raid|persona|test list` — 列出可用场景和测试。
- `forge tui new` / `forge tui resume` — 在工作区上打开交互式 TUI。
- `forge test -test <raid>/<name> [--timeout 2m]` — 运行一条脚本化场景测试。
- `forge test-auto --raid <raid> --persona <persona> [--turns 3]` — 模拟 persona agent 与 raid agent 之间的对话。

## 场景

场景文件是 `scenarios/` 下的普通目录,解析优先级依次为 `--scenarios`、
`FORGE_SCENARIOS`、可执行文件所在目录、当前工作目录、用户配置目录。

### Raid

每个 `scenarios/raids/<name>/` 都是一个完整的工作区模板:

- `deploy.yaml` — `sdkx/deploy` 文档:资源(event bus、scheduler、workspace registry、inference assembly、tool assembly、JS 脚本 runtime)、graph agent 和 runtime integrations。
- `inference.yaml` — provider 配置文件和 secret 解析器。
- `workspace.yaml` — workspace registry 的根目录与布局。
- `tools.yaml` — tool assembly 策略;工具实现是 `internal/simtools` 注册的 Go 值。
- `graphs/assistant.yaml` — graph 定义,同目录下有 `scripts/` 和 `prompts/`(脚本源和 system prompt 通过 `{"file": ...}` 引用)。
- `speakers.yaml` — 可选的每个 graph 节点的用户可见标签;TUI 和测试日志会以 `[主持人]` 这样的标签渲染每个节点的输出。

### 测试

`scenarios/tests/<raid>/<name>.yaml` 定义一条脚本化测试:

```yaml
name: werewolf_opening_setup
description: 开局一盘新的狼人杀,并向用户揭示其 3 号村民身份。
raid: werewolf
turns:
  - 开始狼人杀
```

`forge test` 会把 raid 复制到 `.out/<raid>_<时间戳>/`,逐轮通过 session
runtime 运行,并写出 `stats.txt`(每轮指标,包含失败信息)和 `chat_log.txt`。

### Persona

`scenarios/personas/<name>/` 是完整的 workspace 模板,`forge test-auto`
把它当作模拟中的第二个 agent。

## 凭证

Provider 凭证读取自 `inference.yaml` secret 解析器(`resolver: env`)声明的
环境变量。demo 启动时会加载 forge 目录下的 `.env`,目前里面是
`DEEPSEEK_API_KEY`,所有场景都固定使用 `deepseek-v4-flash`。缺少凭证时应用
会给出明确报错。

## TUI

`forge tui new` 打开两栏 TUI:

- **Chat** — 发送对话并查看流式输出。
- **Workspace** — 工作区元信息和 token 用量。

`Tab` 切换焦点,`Enter` 提交,`Esc` 清空当前输入,连续按两次 `Ctrl+C`
退出。空输入不会提交;输入 `/start` 开启新故事,`/next` 让故事继续推进。
每轮结束后,Chat 面板的输入框下方会显示该轮的 token 统计:输入 /
输出 / 总 token、reasoning token、缓存读 / 写 token 和调用次数。用量通过
`sdkx/runtime` 的 `WithHostFactory` 装饰器从 runtime host 镜像到应用侧;
聚合责任仍在 runtime。

聊天输出按发言人分块:每个 graph 节点的流式文本显示为独立的带标签段落,
工具调用显示为独立的 `[工具调用]` / `[工具结果]` 块,不再混进发言里。

## 与技术栈的接线方式

- `sdkx/deploy` + `sdkx/runtime` 从 `deploy.yaml` 装配 runtime;
  `sdkx/runtime/session` 驱动对话并把流增量发给 sink。
- graph 引擎是 `sdk/graph`,script 节点跑在自带的 JS runtime
  (`sdkx/agent/jsrt`)上。
- 模拟工具是 `internal/simtools` 注册的 Go 值。
- `WithHostFactory` 包住 session host,把每次 LLM 调用的 token 用量镜像到
  应用侧,供 TUI 展示。

## 开发

```bash
go build ./...
go vet ./...
```
