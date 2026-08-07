---
layout: default
title: memory 存储/检索后端抽象方案（Log + KV + SearchBackend）
---
# memory 存储/检索后端抽象方案（Log + KV + SearchBackend）

> **状态**: 草案,待评审。
> **日期**: 2026-08-07
> **作用域**: `memory` 模块内部重构;新增 `memory/storage` 契约、`memory/retrieval.SearchBackend`、`memory/config` 装配改造;`sdk/workspace` 只作为 adapter 被消费,不改其接口形状。
> **前置**: commit `edb9df2d`（workspace 存储契约语义收紧）。
> **兼容性**: `memory` 尚未发布,不保留任何兼容层。`config.Builder` 签名、各 WorkspaceStore 构造、`NewAssembly` 装配全部直接改,不做旧签名/旧构造函数过渡。
> **本期不做**: PG/OpenSearch 后端实现、graph store 抽象、outbox 泛化。

## 0. TL;DR

memory 现在把 `sdk/workspace`（POSIX 文件系统形状）当成唯一存储底座,所有 domain store 都是 `NewWorkspaceStore(ws)` 的实现;检索侧投影又是 workspace-LSM 形状。这导致 PG/OpenSearch 无法接入——PG 被迫模拟文件系统,OpenSearch 被迫消费 delta/digest。

本方案:

1. **契约放 memory 内部**,不放 sdk（单一消费者;workspace 当初的 POSIX 形状就是"过早通用"的教训）。
2. **canonical 底座是两个,不是一个**:`storage.Log`（追加、顺序、幂等）服务 message 及各类事件面;`storage.Store`（KV,前缀扫描）服务当前值/键控状态。两者都有 workspace adapter,未来 SQLite/PG 各一张表实现。
3. **检索契约**改为 item 级 `SearchBackend`（Upsert/Delete/ReplaceAll/Search,分数归一化+统一过滤）;现有 LSM 投影重构为它的第一个实现,未来 OpenSearch 对等接入。
4. **domain 层是具体 struct,不是接口**:producer 侧 domain 接口全部移除（每个都只有一个实现,是投机抽象）;领域语义保留在具体类型中。只有 Log/KV/SearchBackend 是接口,窄视图由消费方自己定义。
5. **config 只描述 driver + settings**;外部实现通过组合根注入实例,或注册 driver 后用名字出现在 config。
6. 所有迁移分阶段独立落地,每阶段保持 `go test ./...` 绿。

## 1. 背景与问题

### 1.1 现状

- `memory/config.Builder` 只持有 `workspace.Workspace` + `inference.Runtime`,`NewAssembly` 对每个 store 调 `NewWorkspaceStore(b.workspace)`。
- memory 非测试代码实际只用 workspace 的 `Read/List/Delete/AtomicWrite`;`Append/Stat/Exists/RemoveAll/Capabilities/Walk/Glob` 零使用。
- `sdk/workspace` 是 POSIX 文件系统形状:路径、目录、mode、mtime。每个新后端（PG/SQLite）被迫模拟文件系统。
- 检索侧 `component.ProjectionDelta` 带 `ActiveIDs/SourceDigest/ReconcileDocuments`,是 workspace-LSM 形状;`Candidate` 泄漏 lane 原生分数。OpenSearch 不关心 delta,它只关心"这批文档是否完整"。
- workspace 还有第二个主人（agent bridge / graph script / sandbox）,memory 改不动它的形状。

### 1.2 结论

- memory 依赖 workspace **接口本身没有问题**,问题是形状。
- workspace 从"存储契约"降级为"存储的一种 adapter"。
- 契约应定义在消费方（memory）内部;将来若出现第二个跨模块消费者,再从 memory 提升到 sdk（单向安全提升）。
- producer 侧 domain 接口（每个 store 一个接口、一个实现）是投机抽象,随实现移除;领域语义保留在具体 struct 中。

## 2. 设计决策

### D1: canonical 底座是 Log + KV 双底座,不是单一 KV

早期草案把 `storage.Store`（KV）当作唯一底座,message 也 rebase 到 KV 上。这要求把"head 指针、幂等 key、批量原子提交、commit 索引"全部手写在 KV 之上（`head.json`/`pending.json`/`commit-index`/零填充 seq/CAS）,等于在 KV 上重造一个日志——与"用文件系统形状模拟数据库"是同一类错误。

盘点后确认:memory 的 store 分三类形状:

| 形状                     | 例子                                                        |
| ------------------------ | ----------------------------------------------------------- |
| 纯追加日志               | message                                                     |
| 日志 + 当前值            | fact、observation、summary                                  |
| 纯 KV（当前值/键控状态） | document 视图、catalog、checkpoint、watermark、repair audit |

所以底座是两个:`Log` 管"追加、顺序、幂等",`Store`（KV）管"按 key 存当前值/键控状态"。

### D2: 检索契约是 item 级 SearchBackend

`Upsert/Delete/ReplaceAll/Search` + 统一过滤 + 归一化分数。LSM 的 delta/digest/repair 留在 memory 内部,从 canonical 计算,不进入接口。

### D3: 契约放 memory,workspace 降级为 adapter

`memory/storage` 定义 Log/KV;`sdk/workspace` 保持现状服务自己的消费者;memory 业务代码不再 import workspace（只有 adapter 包 import）。

### D4: 不引入 graph store 抽象

FlowCraft 的 knowledge 若需要关系检索,先用平行索引/collection,不新增图后端接口。图谱语义可以在现有底座上以平行索引表达,避免为关系检索引入第三种后端抽象。

### D5: 不保留兼容

模块未发布。`config.NewBuilder` 签名、`NewWorkspaceStore` 构造、`NewAssembly` 装配直接改;不提供 deprecated 别名或旧路径。现有 workspace 数据布局直接废弃、不做迁移,本地数据由新布局重新生成。

### D6: domain 层是具体 struct,只有底座是接口

每个 domain store 都只有一个实现,producer 侧接口没有第二个实现可换,属于投机抽象。domain 逻辑（校验、幂等、seq、digest、manifest）保留在具体类型中:`MessageStore`、`FactStore` 等。真正可替换的边界只有 `storage.Log`、`storage.Store`、`retrieval.SearchBackend`。消费者需要窄视图时,在消费方定义小接口（如 `lifecycle.FactReader`）,由具体类型结构性满足;不再由 producer 定义大接口。

## 3. Store 盘点与映射

### 3.1 全部 store

| 现有 store                            | 当前形态                               | 形状                   | 落点                                             |
| ------------------------------------- | -------------------------------------- | ---------------------- | ------------------------------------------------ |
| `sources/message`                     | `Store` 接口 + `WorkspaceStore` 实现   | 纯追加日志             | `MessageStore` 具体类型:Log + KV commit 索引     |
| `sources/document`                    | `Store` 接口 + `WorkspaceStore` 实现   | 当前值 + 事件          | `DocumentStore` 具体类型:KV 当前值 + Log 事件    |
| `sources.ScopeCatalog`                | 接口 + `WorkspaceScopeCatalog` 实现    | 纯 KV                  | `ScopeCatalog` 具体类型:KV                       |
| `views/fact`                          | `Store` 接口 + `WorkspaceStore` 实现   | 只追加 + publication   | `FactStore` 具体类型:Log 事件 + KV 快照          |
| `views/observation`                   | 具体 `WorkspaceStore`（无接口）        | 不可变 + 事件          | `ObservationStore` 具体类型:Log 事件 + KV 快照   |
| `views/summary`                       | `Store` 接口 + `WorkspaceStore` 实现   | 记录 + active manifest | `SummaryStore` 具体类型:Log 记录 + KV manifest   |
| `views/document`                      | `Store` 接口 + `WorkspaceStore` 实现   | 纯当前值               | `DocumentViewStore` 具体类型:KV                  |
| `worker.CheckpointStore`              | 接口 + `WorkspaceCheckpointStore` 实现 | 纯 KV                  | `WorkerCheckpointStore` 具体类型:KV              |
| `lifecycle.CheckpointStore`           | 接口 + `WorkspaceCheckpointStore` 实现 | 纯 KV                  | `LifecycleCheckpointStore` 具体类型:KV           |
| `lifecycle.WorkspaceEventStore`       | 具体类型（无接口）                     | 事件 + 聚合            | `EventStore` 具体类型:Log 事件 + KV 聚合（§5.5） |
| `lifecycle.WorkspaceOutbox`           | 具体类型（无接口）                     | 租约队列状态机         | `Outbox` 具体类型,不泛化（§5.6）                 |
| `lifecycle.WorkspaceRepairAuditStore` | 具体类型（无接口）                     | 不可变审计             | `RepairAuditStore` 具体类型:Log                  |
| `internal/projection.Store[B,D]`      | 泛型具体类型                           | LSM                    | 保留,`SearchBackend` 的 `lsm` 实现内部           |

### 3.2 归属汇总

- **Log**: message、document 事件、fact/observation/summary 事件、recall/visibility 事件、repair audit。
- **KV**: document 当前值、catalog、checkpoint/watermark、document 视图、fact/observation/summary 当前快照、message commit 索引。
- **SearchBackend**: vector/BM25/entity 投影（LSM 实现）,未来 OpenSearch。

### 3.3 抽象处置清单

| 抽象                                                                                                                                                                    | 处置                                                                                                                                                                                                 |
| ----------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `storage.Log`、`storage.Store`（+CAS/Batch）、`retrieval.SearchBackend`                                                                                                 | **保留接口**（唯一可替换边界）                                                                                                                                                                       |
| `message.Store`、`sources/document.Store`、`ScopeCatalog`、`fact.Store`、`summary.Store`、`views/document.Store`、`worker.CheckpointStore`、`lifecycle.CheckpointStore` | **移除 producer 接口**,保留具体类型（`MessageStore` / `DocumentStore` / `ScopeCatalog` / `FactStore` / `SummaryStore` / `DocumentViewStore` / `WorkerCheckpointStore` / `LifecycleCheckpointStore`） |
| `views/observation`                                                                                                                                                     | 不建接口,收敛为 `ObservationStore` 具体类型                                                                                                                                                          |
| `lifecycle.FactReader`、`lifecycle.ObservationStore`、`retrieval.RecallEventRecorder` / `Visibility`                                                                    | **保留为消费者侧窄接口**,由消费方定义,具体类型结构性满足                                                                                                                                             |
| `WorkspaceEventStore`、`WorkspaceOutbox`、`WorkspaceRepairAuditStore`                                                                                                   | 保留具体类型（更名 `EventStore` / `Outbox` / `RepairAuditStore`）,`Outbox` 不泛化                                                                                                                    |
| `internal/projection.Store[B,D]`                                                                                                                                        | 保留为 LSM 物理格式,不对外暴露                                                                                                                                                                       |

## 4. 契约定义

### 4.1 Log（`memory/storage`）

```go
package storage

type Event struct {
    Stream    string          `json:"stream"`
    Seq       uint64          `json:"seq"`
    Type      string          `json:"type"`
    Payload   json.RawMessage `json:"payload"`
    CreatedAt time.Time       `json:"created_at"`
}

type AppendOptions struct {
    IdempotencyKey string            // 同 stream 内唯一;重试返回同一 Commit
    Metadata       map[string]string // 可选
}

type Commit struct {
    ID             string
    Stream         string
    FirstSeq       uint64
    LastSeq        uint64
    IdempotencyKey string
    CreatedAt      time.Time
}

type Log interface {
    // 一次幂等、原子、有序的批量追加;events 非空
    Append(ctx context.Context, stream string, events []Event, opts AppendOptions) (Commit, error)
    // 升序读,after 为排他 seq;limit<=0 表示不限
    Read(ctx context.Context, stream string, after uint64, limit int) ([]Event, error)
    // 按 seq 读单条;缺失 → ErrNotFound
    ReadAt(ctx context.Context, stream string, seq uint64) (Event, error)
    // 最新 n 条,升序返回
    ReadLatest(ctx context.Context, stream string, n int) ([]Event, error)
    // 按前缀列出 stream,字典序;缺失 → 空
    ListStreams(ctx context.Context, prefix string) ([]string, error)
}
```

语义:seq 单调连续由实现保证;`UNIQUE(stream, seq)`、`UNIQUE(stream, idempotency_key)` 是实现约束。批量原子:同一次 Append 要么全部可见,要么全部不可见。

workspace adapter 的"批量原子"是**单进程 + 崩溃恢复**语义(事件与发布指针分离写入,恢复时回滚未发布的批),不是数据库事务;真事务是 SQLite/PG 实现的义务。跨实例并发 append 同一 stream 由 Log 实现保证 seq 唯一,workspace adapter 保持单写者假设。

### 4.2 KV Store（`memory/storage`）

```go
type Entry struct {
    Key   string
    Value []byte
}

type Store interface {
    Get(ctx context.Context, key string) ([]byte, error)   // 缺失 → ErrNotFound
    Put(ctx context.Context, key string, data []byte) error // 原子发布,覆盖即发布
    Delete(ctx context.Context, key string) error           // 幂等;缺失 → nil
    List(ctx context.Context, prefix string) ([]Entry, error) // 前缀扫描,字典序;缺失 → 空
}

// 可选扩展
type CASStore interface {
    CompareAndSwap(ctx context.Context, key string, old, new []byte) (bool, error)
}

type BatchStore interface {
    PutBatch(ctx context.Context, entries []Entry) error
}

// 不可变键(重复写同内容幂等、写不同内容 ErrConflict)必须走 PutIfAbsent;
// Put 的覆盖语义只用于可变当前值。
type PutIfAbsentStore interface {
    // 仅当 key 不存在时写入;已存在则返回 false 且不改动,由调用方决定幂等或 Conflict。
    PutIfAbsent(ctx context.Context, key string, data []byte) (bool, error)
}
```

`List` 是**前缀扫描**（全量 key,字典序）,不是 POSIX 的直接子级;SQLite/PG 的 `key LIKE prefix || '/%' OR key = prefix` 和 workspace adapter 的 `Walk` 都自然满足。前缀按**段边界**匹配（`key == prefix` 或 `key` 以 `prefix + "/"` 开头）,避免分区前缀泄漏到兄弟分区（如 `rt/u/a` 不匹配 `rt/u/ab/...`）。

不可变键（catalog、repair audit、message commit/record、summary record、projection segment）必须使用 `PutIfAbsent`（或等价原语）,否则跨进程并发下无法保证 Conflict 语义。`BatchStore.PutBatch` 要求单次调用要么全部可见要么全部不可见;不能保证的实现不得暴露该接口。依赖 CAS/PutIfAbsent 的用例（§5.5、§5.6）在装配期校验 driver 实现了对应扩展。

### 4.3 SearchBackend（`memory/retrieval`）

```go
type Document struct {
    ID      string
    Text    string
    Vector  []float32
    Payload map[string]any
}

type SearchQuery struct {
    Text      string
    Vector    []float32
    TopK      int
    Threshold float64
    Filters   map[string]any // 统一操作符语言,adapter 翻译
}

type Hit struct {
    ID      string
    Score   float64 // [0,1],越大越相似;归一化是后端义务
    Payload map[string]any
}

type SearchBackend interface {
    Upsert(ctx context.Context, index string, scope sdkmemory.Scope, id string, doc Document) error
    Delete(ctx context.Context, index string, scope sdkmemory.Scope, id string) error
    ReplaceAll(ctx context.Context, index string, scope sdkmemory.Scope, docs []Document) error
    Search(ctx context.Context, index string, query SearchQuery) ([]Hit, error)
}
```

语义:

- 分数归一化是接口义务（L2 → `1/(1+d)`,cosine → `max(0,1-d)`,inner product 原样）;多信号融合/校准在 memory 检索层做,不在后端。
- `ReplaceAll` 是 canonical-first 的一等操作:memory 从 canonical 算出完整文档集后整批重建,不依赖 delta。原子性按 scope 分区承诺:调用方需要跨 scope 全有或全无时逐 scope 调用或依赖后端事务。
- 过滤用统一语言（eq/ne/gt/gte/lt/lte/in/nin/contains/icontains + AND/OR/NOT）,adapter 翻译成各后端语法。
- `Threshold` 是后端对**归一化原生分数**的最小过滤值;融合后的最终阈值由 memory 层决定,不在契约中。
- `index` 只代表投影族（如 `facts`）;scope 是写操作与 `SearchQuery` 的显式参数,各后端翻译（OpenSearch 转 filter clause,LSM 内部按 scope 分区）。
- `Delete` 是 scope-aware 的一等操作;LSM 适配器走 `ProjectionDelta.DeleteIDs`,缺失 id 按 no-op 处理。
- 现有 LSM 投影（vector/BM25/entity 三条 lane）重构为一个 `SearchBackend` 实现（driver 名 `lsm`）;fusion/calibration/rerank 保持在上层。

### 4.4 错误语义

- `ErrNotFound`:Get/ReadAt 缺失;`Delete` 幂等不报。
- `ErrConflict`:幂等 key 重放但内容不一致、`PutIfAbsent` 对已存在 key 的不可变写入。
- CAS 失败:`CompareAndSwap` 返回 `(false, nil)`。
- `PutIfAbsent` 已存在:返回 `(false, nil)`,由调用方决定幂等或 Conflict。
- 驱动配置错误在装配期报 `errdefs.Validation`,不等到运行期。

## 5. 各 store 落地方案

### 5.1 message:纯 Log（第一阶段）

`message.Store` 接口移除,保留具体类型 `MessageStore`。

| MessageStore 方法 | Log/KV 落点                                                                  |
| ----------------- | ---------------------------------------------------------------------------- |
| Append/Commit     | `Log.Append(stream=scope+conversation, events=records, opts.IdempotencyKey)` |
| Get               | `Log.ReadAt`                                                                 |
| List              | `Log.Read(stream, after, limit)`                                             |
| Latest            | `Log.ReadLatest(stream, n)`                                                  |
| ListCommits       | KV 索引 `{scope}/{conv}/commit-index/{version}` → commit ID（或从 Log 派生,见下） |
| ListConversations | `Log.ListStreams(scope 分区前缀)`                                            |

`head.json`/`pending.json` 约定消失,变成 Log 实现内部恢复逻辑;commit 索引收敛为 KV 键（或从 Log 派生）,不再由 MessageStore 手写文件约定。commit 索引与 Log 是两步写:索引只是加速/审计视图,可从 Log 重放重建,缺失不影响 List/Latest,由 repair 工具负责对账。message ID 保持 seq 派生（`msg-%020d`）,不引入额外索引。

### 5.2 纯 KV 组 + 审计

`ScopeCatalog`、`WorkerCheckpointStore`、`LifecycleCheckpointStore`、watermark、`RepairAuditStore`、`DocumentViewStore` 直接改 key 布局:

- catalog:`kv/{scope-partition}/catalog.json`
- checkpoint/watermark:按既有 identity 键
- repair audit:改走 Log（append 即审计）
- document 视图:ReplaceDocument = 写 chunks + 写 build manifest key,再原子更新 pointer key（KV 上的 manifest 最后写语义）

### 5.3 混合组:fact / observation / summary

统一模式:**Log 记事件,KV 存当前快照**。

- fact:publication/merge 事件进 Log;当前 fact 快照在 KV（`facts/{scope}/{id}`）;`ListPublications` 可走 Log 或 KV 索引。
- observation:integrated/superseded/retention/visibility 事件进 Log;`Current` 读 KV 快照（`observations/{scope}/{key}`）。
- summary:不可变 record 进 Log;active manifest 在 KV;压缩只动 manifest,Log 不动。

一致性策略:先 append 事件、后更新快照;快照带 digest,repair 可对 Log 重放校验（与现有一致）。后端支持事务时（PG）同事务提交。

### 5.4 检索投影

`internal/projection.Store[B,D]` 保留为 LSM 物理格式,但只作为 `SearchBackend` 的 `lsm` 实现内部;它的底座从 workspace 改为 KV（manifest/base/segment 都是 KV blob）。memory 业务不再直接看到 `ProjectionDelta/Manifest`。

### 5.5 lifecycle event store

recall/visibility 事件进 Log;Access 聚合是"log + reducer"。实现选 KV+CAS 读改写,或每次从 Log 归约;本期推荐 Log 事件为真、KV 聚合为缓存,归约结果可校验。此点列为待定（§10）。

### 5.6 outbox:不泛化

outbox 是租约状态机（Enqueue/LeaseNext/Complete/Renew/Fail）,不是 KV 也不是日志。保留专用实现（今天 workspace,未来 PG 表）;将来若需 KV 底座,用 KV+CAS 表达租约,但不进 Log/KV 通用契约。

### 5.7 存储命名与编码

- stream 名 = scope hard partition + conversation ID 的确定性编码;同一版本内必须稳定,编码方案是存储细节,但所有实现必须一致可解析。
- workspace adapter:stream/prefix 映射为路径,段用安全编码（沿用现有 `k_` + base64url）,禁止裸用户输入入路径。
- KV key 布局沿用当前分区前缀:`{domain}/{scope-partition}/{...}`;scope partition 用 `HardPartitionKey` 的确定性编码。
- `ListStreams(prefix)` 的 prefix 即 scope 分区前缀,按段边界匹配,实现不得越过分区返回 stream。

## 6. config 与依赖注入

### 6.1 原则

1. **config 是数据**:只描述 `driver + settings`;实例不进 config（延续现有 `json:"-"` 惯例）。
2. **组合根注入实例**是外部实现的主通道;config 是内置/已注册 driver 的通道。
3. **不保留兼容**:`NewBuilder` 旧签名直接删除。
4. **双入口互斥**:组合根注入的 `Backends` 与 settings 的 `storage` 段同时出现时,以注入为准并拒绝 settings 段（Validation）,避免两套真相。

### 6.2 Settings 形状

```go
type BackendSettings struct {
    Driver   string          `json:"driver"`              // 必填,无缺省
    Settings json.RawMessage `json:"settings,omitempty"`  // driver 自己解释
}

type StorageSettings struct {
    Log    BackendSettings `json:"log,omitempty"`
    KV     BackendSettings `json:"kv,omitempty"`
    Search SearchSettings  `json:"search,omitempty"` // lanes: name → BackendSettings
}

type SearchSettings struct {
    Lanes map[string]BackendSettings `json:"lanes,omitempty"`
}
```

`Settings` 增加 `Storage StorageSettings` 字段。config 路径要求显式 driver,不隐藏默认（避免"workspace 悄悄是默认"再次固化为唯一底座）。`search.lanes` 按 lane 名选择 SearchBackend driver;缺省时 NewAssembly 为三条内置 lane 各建一个 `lsm` LaneBackend。

### 6.3 Builder 改造（无兼容）

```go
type Backends struct {
    Log    storage.Log
    KV     storage.Store
    Search retrieval.SearchBackend
}

func NewBuilder(backends Backends, runtime *inference.Runtime) (*Builder, error)
```

`NewAssembly` 不再调任何 `NewWorkspaceStore`;domain 具体类型全部从 `Backends` 构造。所有权约定:注入的 backends 为 borrowed,`Assembly.Close` 只关 runner;由 settings driver 自建的后端（如 pg/sqlite 连接、opensearch client）由创建方（flowcraft Factory / 装配）负责关闭,不得由调用方承担。

### 6.4 driver 注册表

```go
func RegisterLogDriver(name string, factory func(json.RawMessage) (storage.Log, error)) error
func RegisterKVDriver(name string, factory func(json.RawMessage) (storage.Store, error)) error
func RegisterSearchDriver(name string, factory func(SearchDriverDeps, json.RawMessage) (retrieval.SearchBackend, error)) error

type SearchDriverDeps struct {
    KV   storage.Store     // lsm 实现把索引存在 KV 里
    Lane component.Searcher // 被包装的 lane(内置或插件)
}
```

注册通道与现有 `sdk/workspace/config.Builder.RegisterFactory`、`memory/component.Registry` 同套路;`lsm` driver 由 config 用闭包注册(绑定已构建的 lane),OpenSearch 才是 settings 驱动的外部 driver。workspace 需要实例绑定,提供:

```go
// 组合根调用,内部用 ws 构造 Log/KV adapter 并注册到 builder
func RegisterWorkspaceBackends(b *Builder, ws workspace.Workspace) error
```

部署路径中,`driver: workspace` 使用 `in.Dep("workspace")` 的实例绑定 adapter;未提供该 dep 时装配期报 Validation。

内置 driver 名:

| 角色   | 内置名                                                                      |
| ------ | --------------------------------------------------------------------------- |
| Log/KV | `workspace`（需 RegisterWorkspaceBackends）、`sqlite`（未来）、`pg`（未来） |
| Search | `lsm`（未来,基于 KV）、`opensearch`（未来）                                 |

### 6.5 外部实现的三种表达

| 方式                 | config 里写什么                           | 适用                                         |
| -------------------- | ----------------------------------------- | -------------------------------------------- |
| 组合根注入实例       | 不写                                      | 外部代码直接持有 PG/OpenSearch client,最灵活 |
| 注册 driver + config | `{driver: "acme.pglog", settings: {...}}` | 保持声明式部署                               |
| 内置 driver          | `{driver: "pg"}`                          | 官方后端                                     |

### 6.6 示例（未来 PG + OpenSearch）

```yaml
storage:
  log: { driver: pg, settings: { dsn: "...", table: memory_log } }
  kv: { driver: pg, settings: { dsn: "...", table: memory_kv } }
  search:
    { driver: opensearch, settings: { hosts: [...], index_prefix: memory } }
```

同一 PG 实例就是 Log、KV 两张表。装配期校验:driver 未注册 / settings 解码失败 → `errdefs.Validation`。

### 6.7 runtime 集成

`memory/runtime` 继续绑定 `config.Assembly`;未来宿主可通过 `sdkx/runtime` 依赖 kind 注入 `storage.Log` / `storage.Store` / `retrieval.SearchBackend`,config 解析只是构造 Assembly 的来源之一。

### 6.8 部署协议:sdkmemory.Input 只含 Settings + Deps

`sdk/memory` 是能力契约,memory 是它的一个实现;部署协议镜像 inference:

- `sdkmemory.Input` 只有 `{Settings []byte; Deps map[string]any}`,不含 workspace,也不含 inference typed 字段——实现需要什么依赖,自己声明、自己断言。
- `sdk/memory/config.NewDeployFactory(impl, factory, deps ...ResourceDepSpec)`:deps 由实现注册时声明,sdk 不硬编码任何依赖;`New()` 直接转发 `sdk/config.Input`,不做类型断言。
- flowcraft 的 `memory/config.Factory()` 从 `Deps` 断言 `inference`(必需)与 `workspace`(必需;待 Log/KV 落地后,仅当 storage 选 workspace driver 时才需要)。
- 组合根注入实例仍走 `config.NewBuilder(backends, runtime)`,不经部署协议。

与 inference 的对应:host 决定哪些实现代码进二进制,实现自己解码 Settings 并断言 Deps;`sdk/memory/config` 只做搬运。

## 7. 目标目录结构

```
memory/
├── storage/
│   ├── log.go          # Log / Event / Commit / AppendOptions
│   ├── kv.go           # Store / Entry / CASStore / BatchStore
│   ├── errors.go       # ErrNotFound / ErrConflict
│   ├── workspace.go    # WorkspaceLog + WorkspaceKV adapter
│   └── registry.go     # Log/KV driver 注册表
├── retrieval/
│   ├── search.go       # SearchBackend / Document / SearchQuery / Hit
│   └── ...             # fusion / hydrate / pack 保留
├── sources/message/    # MessageStore 具体类型,基于 storage.Log + KV 索引
├── sources/document/   # DocumentStore 具体类型:KV 当前值 + Log 事件
├── views/...           # FactStore / ObservationStore / SummaryStore:Log 事件 + KV 快照
├── projection/...      # vector / bm25 / entity:SearchBackend 的 lsm 实现
└── config/
    ├── backends.go     # Backends / StorageSettings / BackendSettings
    ├── registry.go     # 内置 driver + workspace 绑定注册
    └── build.go        # NewBuilder(backends, runtime)
```

## 8. 实施阶段

每阶段独立合入、可回滚,保持 `go test ./...` 绿（sdk、memory、sdkx）。

### 阶段 1:契约 + workspace adapter(已完成)

- `memory/storage`:Log、KV、错误、CAS/Batch 扩展;`WorkspaceLog`/`WorkspaceKV` adapter。
- 契约测试覆盖:幂等 append、seq 连续性、前缀扫描、Delete 幂等、adapter 与 workspace 语义对齐。

### 阶段 2:message 迁移(已完成)

- `sources/message` 从 `workspace.Workspace` 迁到 `storage.Log` + KV commit 索引。
- `message.Store` 接口移除,保留 `MessageStore` 具体类型;head/pending 文件约定删除,commit 索引收敛为 KV 键（或从 Log 派生）;现有行为测试迁移到新构造。

### 阶段 3:纯 KV 组迁移(已完成)

- catalog、两个 checkpoint、watermark、repair audit（→Log）、document 视图收敛为具体类型。

### 阶段 4:混合组迁移(已完成)

- fact → observation → summary,按"Log 事件 + KV 快照"落地;observation 收敛为 `ObservationStore` 具体类型,不建接口。

### 阶段 5:SearchBackend + config/DI(已完成 5a/5b/5c/5d + lsm LaneBackend 适配器;读路径保持 component.Searcher 插件契约)

实现说明:SearchBackend 契约由每条 lane 的 `LaneBackend` 适配器实现(`Search` 走 `component.Searcher`,`Upsert/Delete/ReplaceAll` 走 `DeltaIndexer/FullRebuilder`,scope 为显式参数)。fusion/provider 读路径**不改调 SearchBackend**,以完整保留 `component.Registry` 的 Indexer/Searcher/Packer 算法插件机制;lane 分数为原生分数,校准仍在上层 fusion。OpenSearch 作为对等 SearchBackend driver 属阶段 6。

- `memory/retrieval.SearchBackend`;LSM 投影重构为 `lsm` 实现（底座换 KV）。
- `config.Backends` + `NewBuilder(backends, runtime)` 替换旧签名;`StorageSettings` + driver 注册表;`RegisterWorkspaceBackends`。
- 收尾清理前序阶段残留的 producer 接口（如 `fact.Store` / `summary.Store` / `views/document.Store`）,消费者改依赖具体类型;窄接口（`FactReader` 等）按需在消费方保留/定义。
- memory 非测试代码不再 import `sdk/workspace`（除 adapter/注册文件）。

### 阶段 6（未来,本期不做）

- SQLite/PG 的 Log/KV 实现、OpenSearch SearchBackend 实现、示例与验证。

## 9. 验收

- memory 业务包（sources/views/worker/lifecycle/retrieval/config）不直接 import `sdk/workspace`。
- 所有现有 store 行为测试在新底座上通过（构造方式更新,断言不变）。
- memory 内 producer 侧 domain 接口清零;接口只剩 `storage.Log` / `storage.Store` / `retrieval.SearchBackend` + 消费者窄接口。
- `storage.Log` 幂等/原子语义、`storage.Store` 前缀扫描、`SearchBackend` 归一化分数有契约测试。
- config 装配:未知 driver、缺 settings 字段均在 `NewAssembly` 报 Validation。
- `make ci`、`git diff --check` 干净。

## 10. 风险与待定

- **快照一致性**:Log 事件与 KV 快照不是单事务时,先事件后快照 + digest 校验;PG 后端可同事务,需在契约注释写明假设。
- **Access 聚合**:KV+CAS 读改写 vs Log 归约,待定;本期以 Log 为真、聚合可重建。
- **outbox**:保留专用实现,不强行抽象;PG 后可用表 + `FOR UPDATE SKIP LOCKED`。
- **List 返回 Value**:KV `List` 携带 value 便于快照扫描,但大 value 扫描成本由实现自行决定（可惰性/分页,契约注释注明）。
- **LSM 与 KV**:`lsm` SearchBackend 依赖 KV 存储段;若未来有更合适的底座,SearchDriverDeps 可扩展。
- **message ID**:seq 派生保持 `msg-%020d`;跨 conversation 唯一性由 stream 前缀保证,文档注明。
- **domain 接口移除的代价**:将来若 PG 要绕过 Log/KV、直接以关系表实现某个 domain store（如需要单事务原子性）,需重新提取接口。模块未发布,提取成本低;若接受"PG 只实现 Log/KV",该风险不存在。
- **workspace adapter 并发与原子性**:Log/KV 的 workspace adapter 是单写者假设,批量原子为"单进程 + 崩溃恢复"而非真事务;跨实例并发与强原子性由 SQLite/PG 提供（见 §4.1）。
- **旧数据不迁移**:现有 workspace 布局直接废弃,本地数据重新生成（见 D5）。
