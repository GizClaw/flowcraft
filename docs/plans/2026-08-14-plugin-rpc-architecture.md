---
layout: default
title: 插件架构（插件壳 + RPC 服务槽）方案
---
# FlowCraft 插件架构方案（插件壳 + RPC 服务槽）

> **状态**: 草案,待评审。
> **日期**: 2026-08-14
> **作用域**: 新增 `core/plugin`（插件壳）、`core/service`（RPC 通道）、`inference.Provider`/rpc 契约适配器、Python 插件 SDK 骨架、示例插件；不改 `resource` / `deploy` / `runtime` / `tool` / `agent` 的既有契约。
> **前置**: `deploy.LoadLayers`（已有，层合并）、`resource.Registry`（已有，注册表）、MCP `tool.Source/mcp`（已有，进程外工具先例）、`core/agent/a2a`（已有，远程引擎协议）。
> **本期不做**: WASM 计算槽（manifest 预留 `wasm` artifact 类型但不实现）、插件签名/加密分发、RPC graph 节点、host.call 回调用面、agent.Engine 插件（A2A 已覆盖）、OCI/远程仓库分发。
> **不替代**: 工具接入继续走 MCP；远程引擎继续走 A2A；新 RPC 协议只面向宿主定义的**资源契约**（v1 锚点：`inference.Provider`）。

## 0. TL;DR

**插件 = 给部署文档可引用的资源契约提供新的 (Kind, Impl) 实现，并贡献声明层。** 宿主定义契约（`resource.Spec` / 各资源 kind），插件提供实现，部署文档选择引用。这与现有架构完全一致：`resource.Factory` 注册表本来就是"核心定义协议、模块提供实现"。

三种槽：

1. **声明层槽**：插件只是一份或多份 `deploy.Layer`，引用宿主已内置的工厂（MCP 工具、已有 provider）——零代码。
2. **服务槽（RPC）**：插件是一个外部进程（Python 等），通过 JSON-RPC 协议提供宿主已定义契约的实现（v1 锚点：`inference.Provider`/rpc）。
3. **原生槽**：现状不变的编译期 Go module 注册。

核心协议一行不改：`resource.Factory`、`deploy.Layer`、`runtime.Builder` 全部原样复用；新增 `core/plugin` 与 `core/service` 两个纯加法包。插件目录来源 = 部署文档顶层 `plugins` 段（§4.1），环境变量等属于上层应用逻辑。分 4 个阶段落地，每阶段独立合入、可回滚。

## 1. 插件定位与边界

### 1.1 插件是什么

- **声明层插件**：一份 `deploy.Layer`，引用宿主已内置的工厂（MCP 工具、provider 等），零代码。
- **服务插件**：一个外部进程（Python 等），通过 §6 的 RPC 协议提供宿主已定义契约的实现。v1 锚点：`inference.Provider`/rpc。
- **原生插件**：Go module，现状。

### 1.2 插件不是什么（边界）

| 不做                       | 原因                                                                                         |
| -------------------------- | -------------------------------------------------------------------------------------------- |
| 工具执行器                 | 执行策略（超时/审批/审计/限流）在宿主 `tool.Executor` + middleware；工具接入走 MCP           |
| agent 引擎整机             | 远程引擎已有 A2A（`agent.Engine`/a2a，HTTP/gRPC、任务轮询、推送）；本地引擎有 graph/scriptrt |
| graph 内核扩展（RPC 节点） | graph 节点是引擎内部执行单元；进程外 handler + board 数据跨进程不属于插件定位                |
| host.call 回调用面         | v1 插件是被调用方；日志走 stderr；需要宿主能力时通过部署文档显式注入                         |

### 1.3 推论

- 插件目录来源 = 部署文档顶层 `plugins` 段（严格解码）；环境变量等是上层应用逻辑，不进插件协议。
- 流式推理是 v1 决策：RPC provider 同时支持 unary 与流式（§7.2）。

## 2. 目标与非目标

### 目标

- 第三方插件**不修改宿主代码、不重新编译宿主**即可接入：声明层槽零代码，服务槽只需要插件作者维护自己的进程。
- 统一插件的发现、校验、版本约束、冲突诊断、生命周期与热加载语义。
- 保持现有架构原则：无全局状态、显式注册、严格解码、错误分类（`errdefs`）。
- 为后续能力预留位置：WASM 计算槽、流式协议、插件市场。

### 非目标（本期）

- WASM 插件加载（`artifacts.type: wasm` 保留为未来枚举值）。
- 插件签名与加密分发；信任交由代码评审 + 发布门禁 + 进程沙箱。
- RPC graph 节点、host.call 回调用面、`agent.Engine` 插件（A2A 已覆盖）。
- 免重启的代码级热替换：服务进程升级需要重建 Runtime 并排空会话（§8）。
- 插件中心 / OCI 分发 / 计费。

## 3. 总体结构

```
plugins/
└── acme.notion-tools/            # 反向域名命名，目录名 = manifest.name
    ├── plugin.yaml               # manifest（必填）
    ├── layers/                   # 可选：声明层（可多个）
    │   └── 10-notion.yaml
    └── service/                  # 可选：RPC 服务产物
        ├── spec.json             # 服务声明（transport/command/env）
        └── python/               # 插件作者自持的实现（python 包等）
```

三种槽：

| 槽     | 产物                | 适合                                               | 先例                                  |
| ------ | ------------------- | -------------------------------------------------- | ------------------------------------- |
| 声明层 | `deploy.Layer`      | MCP 工具、配置片段、策略调整                       | `deploy.LoadLayers`                   |
| 服务槽 | 外部进程 + JSON-RPC | provider、runner 等宿主定义的资源契约（Python 等） | MCP `tool.Source/mcp` 的进程管理      |
| 原生槽 | Go module（现状）   | 需要 cgo/深度集成的原生能力                        | 现有 `Register(r *resource.Registry)` |

## 4. 插件包与 manifest

### 4.1 插件目录来源

部署文档顶层新增 `plugins` 段（与 `resources` / `agents` / `runtime` 平级），由插件加载器严格解码；插件协议不读环境变量：

```yaml
plugins:
  dirs:
    - ./plugins
  enabled: # 可选：显式启用白名单 + 版本约束；缺省 = 全部禁用（2026-08-15 评审确认）
    - acme.notion-tools@^1.0.0
```

### 4.2 manifest（plugin.yaml）

```yaml
name: acme.notion-tools # 反向域名，全局唯一
version: 1.2.0 # semver
description: Notion 集成工具
requires:
  core: ">=0.4.0" # 宿主 core 协议版本约束
  plugins:
    - acme.base@^1.0.0 # 可选：依赖的其他插件
provides: # 能力声明：冲突检测与诊断用
  - kind: inference.Provider
    impl: acme.notion
artifacts:
  - type: layer # 声明层：合并进部署文档
    path: layers/10-notion.yaml
    priority: 100 # 对齐 deploy.Layer.Priority
  - type: service # 服务槽：RPC 能力
    transport: stdio # stdio | http
    command: python
    args: ["-m", "acme_plugin", "--stdio"]
    env: # 凭据注入，不落日志
      NOTION_TOKEN: ${env:NOTION_TOKEN}
    url: "" # http transport 时必填
    headers: {} # http transport 的固定请求头
    protocol_version: 1 # RPC 协议版本
    capabilities: # 可选：本地预声明，用于加载期冲突检测
      - kind: inference.Provider
        impl: acme.notion
```

`artifacts.type` 本期枚举：`layer`、`service`；`wasm` 保留为未来值。manifest 使用 `resource.DecodeTyped` 同款严格解码：未知字段 = 拼写错误。

### 4.3 校验规则

- `name` 为反向域名格式且全局唯一（跨插件集）。
- `version` 为合法 semver。
- `requires.core` 满足宿主 `core` 版本（语义化比较，用 `golang.org/x/mod/semver`，已是间接依赖）。**仅加载期校验**：不匹配即 `Validation` 拒绝（fail-fast），不做运行时协商；版本协商只属于 §6.3 的线上协议。
- `requires.plugins` 中的插件存在且版本约束满足。
- `provides` 的 `(kind, impl)` 不与已注册工厂冲突（复用 `resource.Registry.Register` 的 `Conflict` 语义，错误信息带插件名）。
- `artifacts.layer.path` 可读且严格解码为合法文档片段。
- `artifacts.service` 的 transport 字段合法；stdio 必须有 `command`，http 必须有 `url`。

## 5. `core/plugin`：插件壳

新增 `core/plugin` 包，只依赖 `resource`、`deploy`、`errdefs`，不依赖任何具体资源类型。

### 5.1 Go 接口

```go
package plugin

// Manifest 是插件包目录的静态描述（由 plugin.yaml 严格解码）。
type Manifest struct {
    Name        string
    Version     string
    Description string
    Requires    Requires
    Provides    []resource.Spec
    Artifacts   []Artifact
}

// Plugin 是加载器眼中的最小插件单元。
type Plugin interface {
    Manifest() Manifest
    // Layers 返回插件贡献的配置层（声明层槽）。
    Layers() []deploy.Layer
    // Register 把工厂写进 Target。原生槽直接 import 注册；
    // 服务槽由 core/plugin/remote 的适配器实现。
    Register(ctx context.Context, target *Target) error
    io.Closer
}

// Target 是插件注册能力的落点。
type Target struct {
    Resources *resource.Registry
}

// Loader 扫描目录、解析 manifest、校验、排序、装配。
// 实例持有，无全局状态；同一 Loader 可 Reconcile。
type Loader struct{}

func NewLoader() *Loader
func (l *Loader) Load(ctx context.Context, cfg PluginsConfig, dirs ...string) (*Set, error)
func (l *Loader) Reconcile(ctx context.Context) (*Set, error) // §8

// Set 是一次加载的结果：已激活插件 + 合并后的层序列。
type Set struct {
    Plugins []Plugin
    Layers  []deploy.Layer // 已按 Priority 升序排序
}

func (s *Set) Apply(ctx context.Context, target *Target) error // 逐插件 Register
func (s *Set) Close() error                                    // 逆序 Close 全部插件
```

### 5.2 加载流水线

```
读取部署文档 plugins 段（§4.1）
  → 扫描其声明的插件目录
  → 解析 + 严格校验 manifest（§4.3）
  → 拓扑检查：requires 满足、provides 不冲突
  → 收集 Layers()，按 Priority 升序排序
  → Set.Apply：逐插件 Register(target)
  → 宿主侧：deploy.LoadLayers(主文档层 + set.Layers) → 合并文档
  → runtime.NewBuilder(reg).Build(ctx, mergedDoc)     // 现有代码，零改动
```

错误语义：

- manifest 无效 → `errdefs.Validation`（带插件名与版本）
- `(kind, impl)` 冲突 → `errdefs.Conflict`（带插件名）
- 依赖插件缺失 → `errdefs.NotFound`
- RPC 进程启动失败 / 握手失败 → `errdefs.NotAvailable`（带插件名，可重试）

## 6. `core/service`：RPC 通道

### 6.1 定位

`core/service` 是一个语言无关的进程外插件通道：**进程 supervisor + JSON-RPC 2.0 客户端 + 能力握手**。它不绑定任何具体资源契约；资源契约由 `core/plugin/remote` 里的适配器映射（§7）。

### 6.2 进程与传输

- **stdio**：宿主拉起 `command`（可被 seatbelt/bwrap 包装），协议走 stdin/stdout 的 newline-delimited JSON（JSONL），stderr 进宿主日志；关闭顺序 SIGTERM → 等待 → SIGKILL（对齐 MCP 规范）。
- **http**：JSON-RPC 2.0 POST 到 `url`，固定 `headers`；用于远程部署（插件在别的机器/容器）。
- 超时：每次调用有默认上限（如 30s），可被插件声明覆盖；上下文取消即中止。
- 重试与退避：启动与握手失败按 backoff 重试；稳定运行后崩溃 → 标记 `NotAvailable`，由 Reconcile 或宿主决定重启。
- 载荷上限：请求/响应 JSON 大小默认上限（如 8 MiB），防内存放大。

### 6.3 协议（v1）

宿主是客户端，服务端是插件进程。全部消息为 JSON-RPC 2.0，逐行帧。**v1 无回调用面**：插件是被调用方，日志写 stderr（宿主收集）；需要宿主能力（如事件总线）时通过部署文档的依赖/设置显式注入，而非运行时回调。

**1. 握手（宿主 → 插件）**

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "plugin.handshake",
  "params": {
    "protocol_versions": [1],
    "host_name": "flowcraft",
    "host_core_version": "0.4.0"
  }
}
```

响应：

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "protocol_version": 1,
    "name": "acme.notion",
    "capabilities": [
      {
        "kind": "inference.Provider",
        "impl": "acme.notion",
        "spec": {
          "deps": [],
          "item_type": ""
        }
      }
    ]
  }
}
```

`capabilities` 里的 `spec` 与 `resource.Spec` 同构（kind/impl/deps/item_type），宿主据此注册代理工厂。能力列表以握手结果为准；manifest 里的 `capabilities` 只是本地预声明，用于加载期冲突检测。

**版本协商（2026-08-14 评审确认）**：宿主在握手参数中声明支持的协议版本集合（连续区间，如 `protocol_versions: [1, 2]`），插件在结果中回复它接受并选择使用的**最高公共版本**；无公共版本时握手失败 → `errdefs.NotAvailable`（带插件名）。宿主对已支持的旧版本保留服务能力（对齐 MCP 的协商语义），新版本仅在不兼容变化时引入。manifest 里的 `protocol_version`（§4.2）与 `capabilities` 同理：只是插件声明的目标版本预检查，实际以握手协商为准。

**能力协商（2026-08-14 评审确认）**：宿主按**能力交集**使用插件，插件未声明的能力视为不支持，宿主降级使用而非拒绝（实例见 §7.2 的 unary-only 降级）。

**2. 资源构造与调用**

```json
{"jsonrpc":"2.0","id":2,"method":"resource.new","params":{
  "capability":"inference.Provider/acme.notion",
  "settings": {"id":"notion","model":"gpt-5"}
}}
// result: {"handle":"r-1"}

{"jsonrpc":"2.0","id":3,"method":"resource.call","params":{
  "handle":"r-1",
  "method":"generate",
  "args": {"messages":[...]}
}}
// result: {"content": "...", "usage": {...}}

{"jsonrpc":"2.0","id":4,"method":"resource.close","params":{"handle":"r-1"}}
```

### 6.4 生命周期

`service.Service` 提供：

```go
type Service struct{}

func Start(ctx context.Context, spec ServiceSpec) (*Service, error) // 拉起 + 握手
func (s *Service) New(ctx context.Context, capability string, settings []byte) (string, error)
func (s *Service) Call(ctx context.Context, handle, method string, args []byte) ([]byte, error)
func (s *Service) Close(ctx context.Context) error
func (s *Service) Healthy() bool
```

启动策略：插件 `Apply` 时仅做能力预检查（manifest 声明 vs 本地注册冲突）；首个 `resource.new` 时才真正拉起进程（惰性启动，减少闲置开销）。启动/握手失败映射为 `errdefs.NotAvailable`，`deploy.Builder` 的失败回滚语义原样覆盖（已构造资源逆序 Close）。

## 7. RPC 契约适配器（`core/plugin/remote`）

每个适配器 = 一个宿主侧值，实现对应 Go 接口，方法转发为 `resource.call`。

### 7.1 v1 适配器

| 契约                       | 宿主接口                  | RPC 方法面                                   | 状态                     |
| -------------------------- | ------------------------- | -------------------------------------------- | ------------------------ |
| `inference.Provider`/`rpc` | 提供 `ProviderDefinition` | `generate`、`generate_stream`（http/SSE）    | **v1 锚点**              |
| `sandbox.Runner`/`rpc`     | `sandbox.Runner`          | `exec` 等最小方法集                          | 可选，v1.1（价值待评估） |
| hook                       | `hook.<slot>` 工厂        | `prepare` / `observe` / `referee` / `commit` | 可选，v1.2               |

**v1 模型目录（2026-08-14 评审确认）**：`inference.Provider`/rpc 的模型由部署文档 settings 声明（`id` + `models`/`model`），宿主据此构造 `ProviderDefinition` 的静态模型项（`Dynamic` 字段当前未被模型解析消费）；插件在 `resource.new` 收到同一份 settings。宿主侧模型清单与插件实际能力的一致性由插件在 `generate`/`generate_stream` 内校验。

### 7.2 流式推理（v1 必须决策）

`inference` 有两个执行域：`Generate`（一次返回完整响应）与 `GenerateStream`（增量 part delta：文本/工具调用/推理/音频/图像片段，[generate_stream.go](/Users/haivivi/Workspace/flowcraft/core/inference/generate_stream.go:1)）。流式是 agent 交互的默认体验；RPC provider 若不支持流式，只能用于非交互路径。

决策（2026-08-14 评审确认）：v1 的 `inference.Provider`/rpc 同时支持两者——unary 走 `resource.call`；流式要求 http transport，走独立 SSE 端点（`/stream`），会话句柄由 `resource.new` 建立。插件在握手中声明是否支持流式；未声明的插件降级为 unary-only（仅用于非交互路径，§6.3 能力交集降级原则的实例）。stdio 传输不做流式（JSONL 通知流留 v2，见开放问题）。

### 7.3 代理工厂

```go
// 通用代理工厂：spec 来自握手结果，New 转 resource.new。
type rpcFactory struct {
    service *service.Service
    spec    resource.Spec
    newFn   func(service *service.Service, handle string) any // 包成对应契约值
}

func (f rpcFactory) Spec() resource.Spec { return f.spec }
func (f rpcFactory) New(ctx context.Context, in resource.Input) (any, error) {
    handle, err := f.service.New(ctx, f.spec.Kind+"/"+f.spec.Impl, in.Settings)
    if err != nil { return nil, err }
    return f.newFn(f.service, handle), nil
}
```

代理值实现 `io.Closer`，关闭时调 `resource.close`——`deploy.Builder` 的逆序关闭天然覆盖，无需新机制。

### 7.4 依赖边界

RPC 工厂的 `Spec.Deps` 只允许**文档级可序列化依赖**（settings、其他 RPC 资源句柄）；Go 对象依赖（bus、runner 实例）不跨进程。需要宿主对象的能力不属于插件协议（§1.2），通过部署文档显式配置。

## 8. 热加载与生命周期

诚实区分两类变更：

- **声明层 / 配置变更**：manifest、layer、服务声明变化 → `Loader.Reconcile` diff 后增量生效；工具目录走 MCP `Source.Refresh` 的"失败保留旧投影"模式。
- **代码类变更（服务进程升级）**：需要重建。流程 = 影子构建（新 `resource.Registry` + 合并文档 + `deploy.NewBuilder.Build`）→ 成功后建新 Runtime → 排空旧 Runtime 会话后 `Close`（`Runtime.Close` 已支持等活跃回合结束）。

插件状态机：`discovered → validated → registered → active`；任意一步失败回 `error` 并保留上一可用版本。

**Loader 侧 Reconcile 已落地（2026-08-14）**：`Loader.Reconcile(ctx) (*Set, Changes, error)` 以相同目录/配置重跑装载，按插件指纹（manifest + 层文件内容）diff 出 Added/Removed/Changed；失败保留上一投影（MCP 模式），无变更时返回原 Set。Runtime 级重建与旧会话排空仍由宿主负责（见上）。

## 9. 安全模型

| 槽     | 信任           | 控制                                                                                                                           |
| ------ | -------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| 声明层 | 中（纯配置）   | 严格解码、层来源 provenance、文件大小上限（复用 Loader）                                                                       |
| 服务槽 | 低（外部进程） | 进程沙箱（seatbelt/bwrap 包装 command）、凭据经 env 最小注入且不落日志、调用超时与载荷上限、失败隔离（`errdefs.NotAvailable`） |
| 原生槽 | 高             | 现有代码评审 + 发布门禁                                                                                                        |

## 10. Python 插件 SDK（骨架）

> **2026-08-14 评审确认**：Python SDK 暂缓，先以 Go echo 示例插件（`backends/plugin/example/echo`）作为服务槽协议参考；P1 完成后单独排期。

`contrib/plugin-python/` 提供 pip 包 `flowcraft-plugin`，实现 §6.3 协议：

```python
from flowcraft_plugin import serve, resource

@resource(kind="inference.Provider", impl="acme.notion")
class NotionProvider:
    def generate(self, settings: dict, args: dict) -> dict:
        ...

serve(__name__)
```

职责边界：SDK 只做协议（JSONL 帧、握手、method 分发），业务全在插件侧；pip 依赖由插件作者自己管理（pyproject/venv），宿主只负责拉起 `command`。

## 11. 分阶段落地

| 阶段 | 内容                                                                                                 | 交付物                                      | 验收                                                  |
| ---- | ---------------------------------------------------------------------------------------------------- | ------------------------------------------- | ----------------------------------------------------- |
| P0   | `core/plugin` 壳：manifest、Loader、校验、冲突诊断、层合并、顶层 `plugins` 段解码                    | 包 + 单测 + 一个纯层插件示例                | `plugin.Loader` 加载目录 → 层合并进文档 → 构建通过    |
| P1   | `core/service`：supervisor + JSON-RPC 客户端 + 握手 + 惰性启动                                       | 包 + 单测 + Go echo 示例插件（Python SDK 暂缓，见 §10） | Go 侧构建一个 RPC 资源并调用成功                      |
| P2   | `inference.Provider`/rpc 适配器（unary + http/SSE 流式）                                             | 适配器 + 端到端测试 + Go HTTP provider 示例（Python 示例随 SDK 暂缓） | 部署文档引用 RPC provider，agent 端到端跑通（含流式） ✅ 已落地 `backends/plugin/remote` |
| P3   | 运维化：Reconcile 热加载、http transport 完善、`flowcraft plugin list/validate` CLI、文档、changeset | 命令 + 文档 + `.release`                    | 手册可跟随完成安装/校验/热更新                        |

> **2026-08-14 进度**：Reconcile 热加载（§8 注解）与 http transport 完善（连接池/请求头/非 200 错误分类/空闲连接释放）已完成；CLI、文档、changeset 未做。

每阶段独立合入、可回滚，遵循 Conventional Commits（scope 如 `feat(core/plugin)`）与发布门禁。

## 12. 对既有代码的影响

- **零修改**：`core/resource`、`core/deploy`、`core/runtime`、`core/tool`（MCP 不动）、`core/agent`（A2A 不动）、`core/graph`。
- **纯新增**：`core/plugin`、`core/service`、`core/plugin/remote`、`contrib/plugin-python`、示例插件。
- **落地路径（2026-08-14 评审确认）**：实现先以独立模块 `backends/plugin` 承载（`backends/plugin` 壳、`backends/plugin/service` RPC 通道、`backends/plugin/remote` 适配器），成熟后整包并入 `core`（映射为 `core/plugin`、`core/service`、`core/plugin/remote`）；并入前 `core` 不反向依赖该模块。
- **组合根可选接入**：`examples/forge` 可把手工 `Register` 序列换成 `plugin.Loader`，非强制。
- `core/go.mod` 新增 `golang.org/x/mod`（semver 工具；已是间接依赖，转直接）与现有 backoff 依赖的显式使用。

## 13. 开放问题

1. stdio 传输是否也需要流式（JSONL 通知流），还是 v1 仅 http/SSE 支持流式（建议：仅 http/SSE）。
2. `sandbox.Runner`/rpc 是否值得做：远程沙箱 vs 本地 seatbelt/bwrap 的价值边界不清晰，建议 v1 不做、P2 后单独评审。
3. hook（prepare/observe/referee/commit）RPC 化的优先级与数据面（消息/事件 DTO 的可序列化范围）。
4. ~~插件包的 skill 捆绑（`docs.skill`）~~：**已移除（2026-08-17 评审确认）**——暂无宿主消费且与
   仓库 `skills/` 的开发者文档职责重复；插件系统的使用说明改由仓库 skill
   （`skills/flowcraft-plugin`）承载。若未来需要运行时模型可见的插件说明，
   对齐 MCP skills 等开放约定再议，不再维持私有契约。

## 14. 与 WASM 的关系（本期不做，格式预留）

manifest `artifacts.type` 预留 `wasm`；`core/plugin` 的 `Artifact` 解码器按枚举拒绝未知类型，未来加 wasm 只需新增一个 artifact 解析器 + 一个 `resource.Factory` 包装，壳与流水线不动。
