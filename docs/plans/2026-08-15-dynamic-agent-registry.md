---
layout: default
title: core/runtime 动态增删 agent 方案
---
# core/runtime 动态增删 Agent 方案（动态 Agent 注册表）

> **状态**: 草案,待评审。
> **日期**: 2026-08-15
> **作用域**: 改 `core/runtime`（新增动态 Agent 注册表与 `RegisterAgent` / `UnregisterAgent` API）、`core/runtime/session`（`Manager.RemoveAgent` / 有界关闭）、`core/deploy`（抽取 `BindAgent`）、`core/agent`（`Agent.Close`）；不改 `resource` 契约、不改 `deploy.Result` 的不可变语义、不改 `tool` / `memory` / `inference` / `event` 的既有契约。
> **前置**: `session.InstanceResolver`（已有，窄接口）、`deploy.bindAgents`（已有，agent 装配逻辑）、`tool.Registry.Add/Remove`（已有，运行期增删先例）、`delegation.Directory`（已有，只读绑定）。
> **本期不做**: 免重启的引擎代码热替换（属于插件/Reconcile 范围）、运行时改 `runtime` 配置（max_sessions 等）、跨进程自动拉起 agent（远程引擎继续走 A2A）、delegation 目录的活绑定（作为后续项列出）。

## 0. TL;DR

**现状是静态的：agent 在 `runtime.Builder.Build` 时一次性绑定到 `deploy.Result.agents`（普通 map），`session.Manager` 只持有该 map 的只读快照，`Runtime` 没有增删 API。运行期"动态"的只有 session（按 `Key{AgentID, ContextID}` 懒创建、空闲回收）和构建期解析的 `dynamic_catalog` 工具映射——都不是动态 agent。**

方案：在 `core/runtime` 新增一个**活的 Agent 注册表**（`AgentRegistry`，mutex 保护），实现已有的 `session.InstanceResolver` 接口；`Runtime` 保留构建期的 `resource.Registry` / `Loader` / `deploy.Result` 只读视图，供运行期装配新 agent 时解析 engine/hook factory 与依赖。对外暴露：

```go
func (r *Runtime) RegisterAgent(ctx context.Context, name string, def agent.Definition, opts ...RegisterAgentOption) (*agent.Agent, error)
func (r *Runtime) UnregisterAgent(ctx context.Context, name string, opts ...UnregisterAgentOption) error
func (r *Runtime) Agent(name string) (*agent.Agent, bool)
func (r *Runtime) AgentNames() []string
```

删除语义的关键是并发正确性：在 `session.Manager` 内维护 per-agent tombstone（`removed` 集合），`open` 在持有 `m.mu` 时先查 tombstone，从根上消除"Open 与 Remove 交错导致会话泄漏"的竞态；随后按 `Manager.Close` 的既有模式收集并关闭该 agent 的存量 session（中断活跃 turn + 等待），最后关闭 engine/hooks 并移出注册表。

分 3 个阶段落地，每阶段独立合入、可回滚，遵循 Conventional Commits（scope：`core`）。

## 1. 背景与现状

### 1.1 当前数据流

```
deploy.Document（静态）
  → runtime.Builder.Build
    → deploy.Builder.Deploy：bindAgents 把每个 Definition 装配成 *agent.Agent
      写入 deploy.Result.agents（普通 map，无锁、无增删方法）
    → session.NewManager(resultResolver{result}, ...)
       resultResolver.Instance(id) = result.Agent(id)   // 只读快照
  → Runtime{manager, router, result}
```

关键代码位置：

- agent 装配：`deploy.bindAgents`（[builder.go](/Users/haivivi/Workspace/flowcraft/core/deploy/builder.go:207)），结果写入 `result.agents[name]`（[builder.go](/Users/haivivi/Workspace/flowcraft/core/deploy/builder.go:263)）。
- 只读快照：`resultResolver`（[builder.go](/Users/haivivi/Workspace/flowcraft/core/runtime/builder.go:206)）与 `Result.Agent`（[builder.go](/Users/haivivi/Workspace/flowcraft/core/deploy/builder.go:437)）。
- 会话懒创建：`Manager.open` 在持有 `m.mu` 时调 `resolver.Instance(key.AgentID)`，不存在则返回 `NotFound`（[manager.go](/Users/haivivi/Workspace/flowcraft/core/runtime/session/manager.go:124)）。
- 空闲回收：`Manager.reclaim` 删除条目并 `session.close()`（[manager.go](/Users/haivivi/Workspace/flowcraft/core/runtime/session/manager.go:231)）。
- 动态工具目录：`resolveDynamicCatalogAssemblies` + `newDynamicCatalogProvider` 在构建期把 `agentID → tool.Assembly` 固化成 map（[dynamic.go](/Users/haivivi/Workspace/flowcraft/core/runtime/dynamic.go:26)）。
- 运行期增删先例：`tool.Registry.Add/Remove`（Remove 会对 `io.Closer` 释放资源，[registry.go](/Users/haivivi/Workspace/flowcraft/core/tool/registry.go:114)）。

### 1.2 现状结论

- `deploy.Result` 是部署期产物，`agents` 无并发保护、无增删方法；`Runtime` 不保留 `resource.Registry` / `Loader`，无法事后装配新 agent。
- `session.Manager` 通过窄接口 `InstanceResolver` 解析 agent——**这是最顺的接缝**，替换实现即可让新 agent 可被会话使用。
- `Session.close` 已有"中断活跃 turn + 等待收尾"的完整语义（[session.go](/Users/haivivi/Workspace/flowcraft/core/runtime/session/session.go:654)），删除 agent 时可直接复用；唯一缺口是等待用 `context.Background()`，需要改为可被调用方 ctx 约束。
- agent 的 engine/hooks 目前**没有关闭路径**：`deploy.Result.Close` 只关资源值，不关 `*agent.Agent` 的 Engine/hook（[builder.go](/Users/haivivi/Workspace/flowcraft/core/deploy/builder.go:452)）。动态删除必须补上，否则删掉的 agent 泄漏引擎资源。
- `delegation.LocalDirectory` 绑定一次后不可变（[directory.go](/Users/haivivi/Workspace/flowcraft/core/delegation/directory.go:37)），动态 agent 默认不会成为委托目标——本期文档化该限制，活目录列为后续项。

## 2. 目标与非目标

### 目标

- 不重建 `Runtime`、不重启进程的前提下，注册一个新 agent 并立即能对它 `Manager.Open` / `GetOrCreate` 开会话、跑 turn。
- 删除一个 agent：新会话被拒绝，存量会话与活跃 turn 有序收尾，engine/hooks 释放，再次注册同名 agent 可用。
- 与动态工具目录联动：新 agent 可携带工具装配（`tool.Assembly`）或回退到 `default`。
- 保持现有架构原则：无全局状态、显式注册、严格校验、错误分类（`errdefs`）、`deploy.Result` 不可变。
- 并发正确：Open 与 Remove 任意交错不泄漏会话、不悬挂 turn、不出现"删除后仍可开新会话"。

### 非目标（本期）

- 免重启的引擎/hook 代码级热替换（plugin/Reconcile 范围，见既有插件方案）。
- 运行时修改 `runtime` 配置（idle_timeout、max_sessions 等）或事件总线拓扑。
- 让动态 agent 自动出现在 `delegation.Directory` 中（只读目录 v1 不做活绑定）。
- 跨进程拉起新 agent 进程（远程引擎用 A2A，本地引擎 factory 仍是构建期注册的 `resource.Factory`）。

## 3. 总体结构

```
                      runtime.Runtime
        ┌─────────────────┼──────────────────┐
        │                 │                  │
   deploy.Result      resource.Registry   session.Manager
   （只读：资源值        （保留，供        （持有 tombstone；
    / 依赖解析）         RegisterAgent       新增 RemoveAgent）
         │              装配 engine/hooks）
         │                 │                  │
         └──────► AgentRegistry（活的）◄──────┘
                   ├─ map[string]*agent.Agent    // 并发安全
                   ├─ map[string]*tool.Assembly  // 动态目录（活的）
                   └─ implements session.InstanceResolver
```

要点：

- `AgentRegistry` 是 `core/runtime` 内新增包内类型，实现 `session.InstanceResolver`（只多一个方法面：`Instance(id)`），替代 `resultResolver` 交给 `Manager`。
- `deploy.Result` **保持不可变**：它是部署期快照；动态增删只发生在 `AgentRegistry` 这一"活"层，避免把运行期可变性引入部署产物。
- `Runtime` 持有 `resource.Registry`（builder 现在用完即丢，需要保留）与 `Loader`，`RegisterAgent` 时复用部署期同一套 factory 与依赖解析逻辑。
- 删除并发安全由两层协作：`AgentRegistry` 负责"发现"（`Agent/AgentNames/Instance`），`session.Manager` 内的 tombstone 负责"开关"（阻止新会话打开）。

## 4. API 设计

### 4.1 `core/runtime`：Runtime 对外 API

```go
// RegisterAgentOption 配置一次动态注册。
type RegisterAgentOption func(*registerOptions) error

// WithToolAssembly 为动态 agent 指定工具装配资源（部署文档 resources
// 中已构建的 *tool.Assembly 名字）。未指定时回退 dynamic_catalog 的
// default；dynamic_catalog 已配置但无 default 时，注册必须显式携带该选项。
func WithToolAssembly(resourceName string) RegisterAgentOption

// RegisterAgent 装配并注册一个新 agent。name 即 agent ID，必须非空、
// 无首尾空白。同名已注册 → Conflict；同名曾删除（tombstone）→ 允许重注册。
func (r *Runtime) RegisterAgent(ctx context.Context, name string, def agent.Definition, opts ...RegisterAgentOption) (*agent.Agent, error)

// UnregisterAgentOption 配置一次删除。
type UnregisterAgentOption func(*removeOptions) error

// WithRemoveTimeout 约束删除总耗时（含活跃 turn 的收尾等待）；缺省无界。
func WithRemoveTimeout(d time.Duration) UnregisterAgentOption

// UnregisterAgent 删除一个动态注册的 agent：阻塞新会话 → 排空存量会话
//（等待活跃 turn 自然结束）→ 移出注册表 → 关闭 engine/hooks。
// 未知名字 → 幂等 nil（对齐 tool.Registry.Remove）；静态部署 agent → Conflict。
func (r *Runtime) UnregisterAgent(ctx context.Context, name string, opts ...UnregisterAgentOption) error

// Agent / AgentNames 是"活"的发现视图（区别于 deploy.Result 的部署快照）。
func (r *Runtime) Agent(name string) (*agent.Agent, bool)
func (r *Runtime) AgentNames() []string
```

`Runtime` 结构新增字段：`registry *AgentRegistry`（活的）、`resources *resource.Registry`（保留）、`loader *resource.Loader`（保留）、`liveCatalog *catalogRegistry`（活的工具目录，见 §6）、`bus event.Bus`（生命周期事件用）、`lifecycleMu sync.Mutex` + `closed bool`（操作互斥，见 §5.4）。`Builder.Build` 结束时把这些与 `deploy.Result` 一起交还给 `Runtime`。

### 4.2 `core/runtime`：AgentRegistry

```go
// AgentRegistry 是运行期可变、并发安全的 agent 视图。
type AgentRegistry struct {
    mu       sync.RWMutex
    agents   map[string]*agent.Agent // 动态注册的 agent
    deployed *deploy.Result          // 只读回退视图（静态部署 agent）
}

func (r *AgentRegistry) Instance(id string) (*agent.Agent, bool) // session.InstanceResolver
func (r *AgentRegistry) Agent(id string) (*agent.Agent, bool)
func (r *AgentRegistry) AgentNames() []string
func (r *AgentRegistry) Put(id string, a *agent.Agent) error   // 重复 → Conflict
func (r *AgentRegistry) Delete(id string) (*agent.Agent, bool)
func (r *AgentRegistry) Close() error                          // 关闭剩余动态 agent
```

`Agent/Instance/AgentNames` 先查动态 map，再回退 `deployed`，因此 `Runtime.Agent` 是"部署快照 + 动态层"的合并视图。`Delete/Close` 只作用于动态 map——静态 agent 的生命周期仍归 `deploy.Result` 所有，杜绝双关。

### 4.3 `core/deploy`：抽取 BindAgent

把 `bindAgents` 循环体抽成可复用的导出函数，构建期 `bindAgents` 与运行期 `RegisterAgent` 共用同一装配路径（engine factory `New` → hooks 构建 + `Wire` → 组装 `*agent.Agent`）：

```go
// BindAgent 用注册表中的 factory 装配单个 agent，返回运行形态。
// 不修改 result（不动 result.agents）；依赖解析只读 result.values。
func BindAgent(
    ctx context.Context,
    reg *resource.Registry,
    result *Result,
    loader *resource.Loader,
    name string,
    def agent.Definition,
) (*agent.Agent, error)
```

`bindAgents` 重构为循环调 `BindAgent` 再 `result.agents[name] = a`，行为不变。

### 4.4 `core/agent`：Agent.Close

```go
// Close 释放 agent 持有的引擎与钩子（实现 io.Closer 的组件），
// 逐项 close 并 errors.Join 汇总。幂等由各组件自身保证。
func (a *Agent) Close() error
```

同时修复一个既有的资源泄漏：`deploy.Result.Close` 在逆序关闭资源值后，对每个 `agents` 成员调用 `a.Close()`。这是行为增强，不破坏现有调用方（此前 engine/hooks 从没被关闭过）。

`BindAgent` 内部做部分装配回滚：engine 已构造、后续 hook 构造/Wire 失败时，按逆序关闭已构造的 engine 与 hooks（实现 `io.Closer` 的组件），避免运行期注册失败泄漏已 Wire 的订阅。

### 4.5 `core/runtime/session`：Manager 增删与排空

```go
// RemoveAgent 阻塞该 agent 的新会话打开，排空其存量会话：
//   - 等待所有活跃 turn 自然结束（有界，ctx 控制）；
//   - 全部 idle 后关闭会话（此时 close 不等待，安全）；
//   - ctx 超时/取消 → 返回 DeadlineExceeded/Canceled，tombstone 保留，
//     会话原样保留，调用方可重试（幂等收尾剩余会话）。
func (m *Manager) RemoveAgent(ctx context.Context, name string) error

// ReopenAgent 清除 RemoveAgent 留下的 tombstone，供重注册同名 agent。
func (m *Manager) ReopenAgent(name string)
```

**为什么是"排空"而不是"中断 + 等待"**：有界等待"中断后的 turn 退出"存在安全死角——若 ctx 超时，`close` 返回但 turn goroutine 可能仍存活，此时关闭 engine 是 use-after-free。排空方案把"有界等待"放在 turn **自然结束**上，超时即放弃本次删除（不拆机），从根上消除部分删除状态；会话全部 idle 后的关闭是无等待的，天然安全。

`Manager.RemoveAgent` 不需要改 `Session.close` 签名（保持无参、中断 + 无界等待的既有语义），仅新增：

```go
// Manager 新增字段
removed map[string]struct{} // tombstone：阻塞该 agent 的新会话

// open() 在 m.mu 内新增一行（见 §5.2）
// release / activityChanged / scheduleIdleTimerLocked / reclaim 对
// removed 中的 agent 跳过空闲回收，避免排水期间会话被提前回收。
```

### 4.6 注册示例

```yaml
# 一段可注入的 agent config（与部署文档 agents.<name> 同构）
card:
  name: Ticket QA
  description: 评审工单改动
  skills: [{id: qa, name: QA, description: 代码评审}]
engine:
  kind: agent.Engine
  impl: graph
  settings: {graph: ticket-qa}
policy: {max_revise: 1}
tools: [search, git]
observe:
  - type: audit
    settings: {channel: ticket-events}
```

```go
var def agent.Definition // 上面的 YAML 严格解码（同部署文档解码路径）
instance, err := app.RegisterAgent(ctx, "ticket-qa",
    def,
    runtime.WithToolAssembly("kit"), // 可选：动态工具目录
)
if err != nil { /* Validation/Conflict/NotFound ... */ }

lease, err := app.Sessions().GetOrCreate(ctx, session.Key{
    AgentID: "ticket-qa", ContextID: "user-7",
})
// ... Start / Wait，与静态 agent 完全一致

if err := app.UnregisterAgent(ctx, "ticket-qa",
    runtime.WithRemoveTimeout(30*time.Second),
); err != nil { /* DeadlineExceeded: 可重试 */ }
```

## 5. 删除语义与并发正确性

### 5.1 状态机

```
active ──UnregisterAgent──► removing ──关闭全部会话/释放资源──► removed
   ▲                                                            │
   └──────────────── ReopenAgent + RegisterAgent ◄──────────────┘
```

- `removing`：新会话打开被拒（tombstone 生效）；发现视图暂不列出（若排空失败，agent 会被放回）。
- `removed`：发现视图（`Agent` / `AgentNames`）不再列出；engine/hooks 已释放。
- 重注册：`RegisterAgent` 先装配成功，再 `ReopenAgent` + `Put`，避免失败时留下"半开"状态。

### 5.2 竞态分析（Open vs Remove）

若删除只做"注册表 delete + 收集会话"，存在会话泄漏的交错：

1. `open` 持有 `m.mu` 解析 `resolver.Instance(id)` 成功（此刻删除尚未发生）。
2. 删除线程删注册表、收集会话（此时条目尚未插入）并解锁。
3. `open` 插入新会话后解锁——该会话指向已删除的 agent，且永远不会被删除流程关闭。

修复：**tombstone 放在 `Manager` 内、检查发生在 `m.mu` 之下**。`open` 的流程变为：

```go
m.mu.Lock()
defer m.mu.Unlock()
if m.closed { ... }
if _, gone := m.removed[key.AgentID]; gone {
    return nil, errdefs.NotFoundf("runtime session: agent %q is not deployed", key.AgentID)
}
// ... 既有 entries 命中 / resolver 解析 / 插入
```

`RemoveAgent` 在同一个 `m.mu` 内先写 tombstone（幂等），随后在锁外**排空**（等待 idle），最后关闭并清理条目：

```go
func (m *Manager) RemoveAgent(ctx context.Context, name string) error {
    if isNil(ctx) { return errdefs.Validationf("runtime session: context is required") }
    m.mu.Lock()
    if m.closed { m.mu.Unlock(); return ErrManagerClosed }
    m.removed[name] = struct{}{}          // 幂等：重试时保留
    for key, entry := range m.entries {
        if key.AgentID != name { continue }
        entry.idleGeneration++             // 使迟到的 reclaim 回调失效
        if entry.timer != nil { entry.timer.Stop(); entry.timer = nil }
    }
    var sessions []*Session
    for key, entry := range m.entries {
        if key.AgentID == name { sessions = append(sessions, entry.session) }
    }
    m.mu.Unlock()

    // 排空：等待所有活跃 turn 自然结束（有界）。超时即放弃，
    // 不关闭任何会话、不产生部分删除状态。
    if err := m.awaitAgentIdle(ctx, name); err != nil { return err }

    // 全部 idle 后关闭：close 对 idle 会话无等待，安全。
    for _, s := range sessions { s.markClosing() }
    for _, s := range sessions { s.notifySessionClosing(true) }
    var errs []error
    for _, s := range sessions { if err := s.close(); err != nil { errs = append(errs, err) } }

    m.mu.Lock()
    for key := range m.entries {
        if key.AgentID == name { delete(m.entries, key) }
    }
    m.mu.Unlock()
    return errors.Join(errs...)
}
```

`awaitAgentIdle` 以短周期轮询 `agentIdle(name)`（所有 `Key.AgentID == name` 的会话均 `isIdle()`）并 select `ctx.Done()`；`release / activityChanged / scheduleIdleTimerLocked / reclaim` 对 `removed` 中的 agent 一律跳过（不调度、不回收），保证排空期间会话不被并发回收。排空通过后到关闭之间，若某会话被持有 lease 的调用方启动了新 turn，`Session.close` 的中断 + 无界等待语义兜底，仍然安全。

因为 `open` 的"检查 tombstone → 解析 → 插入"全程持有 `m.mu`，而 `RemoveAgent` 的"写 tombstone → 收集"也在 `m.mu` 内，二者互斥后必居其一：

- `open` 先完成：会话已插入，`RemoveAgent` 收集时可见并被关闭；
- `RemoveAgent` 先完成：`open` 在 tombstone 检查处被拒。

不存在中间态。`reclaim` 竞态同理：条目已被 `RemoveAgent` 删除，迟到的 reclaim 回调在 `m.mu` 内检查条目不存在即空操作（既有逻辑已覆盖）。

### 5.3 Runtime.UnregisterAgent 的清理顺序

```go
func (r *Runtime) UnregisterAgent(ctx context.Context, name string, opts ...UnregisterAgentOption) error {
    r.lifecycleMu.Lock()               // 与 RegisterAgent / Close 互斥（§5.4）
    defer r.lifecycleMu.Unlock()
    if r.closed { return errdefs.NotAvailablef("runtime: closed") }

    // 0. 定位：只允许删除动态 agent
    instance, ok := r.registry.Delete(name)
    if !ok {
        if _, deployed := r.result.Agent(name); deployed {
            return errdefs.Conflictf("runtime: agent %q is deployed; remove it from the document", name)
        }
        return nil // 未知名字：幂等
    }

    // 1. 阻塞新会话 + 排空存量会话（ctx 有界等待 turn 自然结束）
    if err := r.manager.RemoveAgent(ctx, name); err != nil {
        // 超时/取消：tombstone 保留（新会话仍被拒），agent 仍在注册表，
        // engine/hooks 未释放（turn 可能仍存活）。调用方可重试。
        r.registry.Put(name, instance)
        return err
    }
    // 2. 移出动态工具目录
    if r.liveCatalog != nil { r.liveCatalog.Delete(name) }
    // 3. 释放 engine/hooks（此时已无会话引用该 agent）
    return instance.Close()
}
```

顺序原则：**先断电（堵新会话）、再排水（等 turn 结束）、最后拆机（释放资源）**。`RemoveAgent` 失败时把 agent **放回注册表**（排空期间它暂时离开发现视图，失败即恢复），保证失败重试的语义干净；只有全部会话确认关闭后才允许 `Close` engine/hooks。

### 5.4 操作互斥（Register / Unregister / Close）

`Manager` 内的 tombstone 只解决 `Open vs Remove`；**`Register` 与 `Remove` 之间仍需一个更高的互斥**，否则存在覆盖竞态：

1. `UnregisterAgent` 执行 `manager.RemoveAgent`（tombstone 已写入）。
2. 并发 `RegisterAgent` 构建成功 → `ReopenAgent` 清 tombstone → 新 agent `Put`。
3. `UnregisterAgent` 继续 `registry.Delete`——删掉并 Close 了刚注册的新 agent。

修复：`Runtime` 持有 `lifecycleMu sync.Mutex`，`RegisterAgent` / `UnregisterAgent` / `Close` 全程持有（控制面 QPS 低，全局锁足够）。`Runtime.Close` 顺序：抢 `lifecycleMu` → 置 `closed` → `manager.Close` → `router.Close` → `result.Close`（静态 agent）→ `registry.Close`（动态 agent）；持锁期间任何增删调用先等锁、再撞 `closed` 返回 `NotAvailable`。`RegisterAgent` 的提交顺序固定为：`ReopenAgent` → `liveCatalog.Set` → `registry.Put` → 发布事件，全程在 `lifecycleMu` 内，不存在中间可见状态。

## 6. 动态工具目录联动

现状的 `newDynamicCatalogProvider` 捕获构建期 map，运行期不可变。改造为活的 `catalogRegistry`：

```go
// catalogRegistry 是 agentID → tool.Assembly 的活映射，兼容 default 回退。
type catalogRegistry struct {
    mu         sync.RWMutex
    assemblies map[string]*tool.Assembly
    def        *tool.Assembly // 构建期 dynamic_catalog 的 default
}

func (c *catalogRegistry) NewCatalog(ctx context.Context, instance *agent.Agent) (tool.Session, error)
func (c *catalogRegistry) Set(id string, a *tool.Assembly)
func (c *catalogRegistry) Delete(id string)
```

规则：

- 构建期把 `resolveDynamicCatalogAssemblies` 的结果灌入 `catalogRegistry`（行为不变）。
- `RegisterAgent` 带 `WithToolAssembly("resourceName")` 时，用 `deploy.Result.Value` 解析已构建的 `*tool.Assembly` 并 `Set`；未指定时回退 `default`；`dynamic_catalog` 已配置但无 `default` 且未显式指定 → 注册期 `Validation`（对齐构建期"每个 agent 必须有映射或 default"的规则）；`dynamic_catalog` 未配置时 agent 无工具目录（与现状一致）。
- `UnregisterAgent` 调用 `Delete`；`default` 本身不可删除。

## 7. 事件与可观测性（P2，可选）

新增运行期生命周期主题（`core/runtime/events.go`），供 UI/运维订阅：

```
runtime.agent.<id>.registered
runtime.agent.<id>.removed
```

载荷仅包含 `agent_id`、`name`、`description`（从 `AgentCard` 取，避免事件放大）。删除失败（排空超时）不发布 `removed`，只发布 `removed` 成功事件。事件经 `Runtime.Attach` 可订阅，与现有 `agent.run.*` 命名空间并存。**不阻塞注册/删除主流程**：发布失败仅记日志。

## 8. 与既有机制的关系

| 机制 | 关系 | 动作 |
| --- | --- | --- |
| `session.Manager` | 窄接口 `InstanceResolver` 替换实现 | 新增 tombstone + `RemoveAgent/ReopenAgent`，`open` 增加一行检查 |
| `deploy.Result` | 部署期快照，保持不可变 | 不改；动态层独立为 `AgentRegistry` |
| `dynamic_catalog` | agent → tool assembly 构建期映射 | 变活：`catalogRegistry`，行为兼容 |
| `delegation.Directory` | 绑定一次后不可变 | 本期文档化限制：动态 agent 不作为委托目标；活目录列后续项 |
| `memory` | 按 `AgentID` 硬分区 | 无需改动；动态 agent 天然可写 |
| checkpoint/resume | 按 runID 存储，不按 agent 索引 | 删除 agent 后其 parked run 无法恢复（会话已关）；存储清理属开放问题 |
| `tool.Registry.Add/Remove` | 运行期增删先例 | 本方案的删除顺序与其一致（先移出、后释放 `io.Closer`） |
| 插件 / A2A | 引擎仍是构建期注册的 `resource.Factory` | `RegisterAgent` 只引用既有 factory，不引入新的引擎加载机制 |

## 9. 对既有代码的影响与兼容性

- **零破坏**：`deploy.Result` 公开面不变；`Manager.Open/GetOrCreate` 签名不变；`Session.close` 签名不变。
- **行为增强**：`deploy.Result.Close` 新增关闭 agents（修复 engine/hooks 泄漏）；`dynamic_catalog` 内部从静态 map 换为活映射，对外语义不变。
- **错误分类**：重复注册（含与静态 agent 同名）→ `errdefs.Conflict`；未知删除 → 幂等 nil（对齐 `tool.Registry.Remove`）；删除静态部署 agent → `Conflict`；构建/装配失败 → `Validation`；排空超时 → `DeadlineExceeded` 归类（agent 放回注册表，可重试）。
- **构建门禁**：按仓库规范跑 `make ci`、`make lint`、`git diff --check`；发布时才加 `.release/core.json` changeset（summary 覆盖自上一 tag 的全部累积变更）。

## 10. 分阶段落地

| 阶段 | 内容 | 交付物 | 验收 |
| --- | --- | --- | --- |
| P0 | 抽取 `deploy.BindAgent`（含部分装配回滚）；`agent.Agent.Close`；`deploy.Result.Close` 关闭 agents | 重构 + 单测 | 既有 `core/deploy`、`core/runtime` 测试全绿；`Result.Close` 关闭引擎、`BindAgent` 中途失败回滚测试通过 |
| P1 | `AgentRegistry` + `catalogRegistry`；`Manager.RemoveAgent(ctx,name)/ReopenAgent` + tombstone + 排空；`Runtime.RegisterAgent/UnregisterAgent/Agent/AgentNames` + options + `lifecycleMu` | API + 单测 + 并发竞态测试 | 动态注册→开会话→跑 turn 端到端通过；Open vs Remove、Register vs Remove 交错测试无泄漏、无覆盖 |
| P2 | 生命周期事件（§7）；文档（本方案归档进 guides）；`examples/forge` 动态增删示例（受 GOWORK=off 固定版本约束，随下次 core 发布落地）；`.release/core.json`（仅实际发布时添加） | 事件 + 文档 + 示例 + changeset | 手册可跟随完成"运行期加一个 agent → 对话 → 删除 → 重加" |

每阶段独立合入、可回滚。

> **2026-08-15 进度**：P0、P1 与 P2 的生命周期事件已实现并通过 `core` 模块全量测试（`go test ./...`，含 `-race` 并发用例：Open vs Remove、Register vs Remove、Close vs Register）。`examples/forge` 动态增删示例与 `.release/core.json` changeset 按发布流程延后（`examples/` 以 `GOWORK=off` 固定版本构建，需等 core 实际发布后落地）。

## 11. 测试与验收

### P1 核心测试矩阵

- `RegisterAgent` 成功 → `Manager.GetOrCreate` 开新会话 → `Start` 跑 turn 成功。
- 重复注册同名 → `Conflict`。
- `UnregisterAgent` 后 `Open` → `NotFound`；`Agent/AgentNames` 不再列出。
- 删除时存在活跃 turn → 排空等待其自然结束；engine `Close` 被调用。
- 排空超时（turn 不结束 + `WithRemoveTimeout` 到期）→ `DeadlineExceeded`，agent 放回注册表，engine **未被** Close，重试后成功。
- 删除后重注册同名 → 可再次开会话（tombstone 清除）。
- `WithToolAssembly` 解析失败 → `NotFound`；缺省回退 `default`。
- 动态 agent 无 `dynamic_catalog` 配置时 → 无工具目录，不报错。
- `BindAgent` 中途失败（hook factory 报错）→ 已构造的 engine/hooks 被逆序 Close。
- 删除静态部署 agent → `Conflict`；删除未知名字 → 幂等 nil。

### 并发竞态测试（关键）

- `N` 个 goroutine 并发 `Open` + `M` 个 goroutine 并发 `RemoveAgent` 同 agent：最终不存在"已删除 agent 的会话"且无泄漏；用 `-race` 跑。
- `RemoveAgent` 与 `reclaim` 并发：迟到的 idle timer 不得复活已删除条目。
- `RegisterAgent`（重注册）与 `Open` 并发：`Open` 要么在 tombstone 清除前被拒，要么成功后拿到新 agent。
- `RegisterAgent` 与 `UnregisterAgent` 并发同 agent（`lifecycleMu` 下）：串行化，不出现"删掉刚注册的新 agent"覆盖。
- `Runtime.Close` 与 `RegisterAgent` 并发：`RegisterAgent` 等锁后返回 `NotAvailable`，无泄漏。

### 门禁

`make ci`、`make lint`、`git diff --check`；`core` 模块发布前 `make release-plan` 确认 changeset 覆盖累积变更。

## 12. 开放问题

1. ~~`UnregisterAgent` 对未知名字：`NotFound` 还是幂等 `nil`？~~ **已定：幂等 nil**（与 `tool.Registry.Remove` 一致）。
2. ~~删除时的收尾策略：中断 + 等待 vs drain？~~ **已定：drain（等自然结束）**，超时即放弃本次删除、不拆机；中断式删除作为后续项（需要 turn 强制退出机制）。
3. checkpoint 清理：`CheckpointStore` 按 runID 索引、无按 agent 批量删除接口；被删 agent 的存量 checkpoint 是否要新增按 agent 清理？（建议暂不，交给存储侧保留策略。）
4. `delegation.Directory` 活绑定是否值得做：动态 agent 作为委托目标的价值与实现成本（需要把只读 `Deployment` 视图换成可刷新目录）？（建议 P2 后单独评审。）
5. ~~动态 agent 的事件载荷是否要包含完整 `AgentCard`？~~ **已定：只给 `agent_id` + `name` + `description`**（避免事件放大）。
