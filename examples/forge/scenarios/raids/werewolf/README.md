# 狼人杀场景重构方案

> 状态：实现完成，已通过本地烟雾测试（开局 → 夜晚 → 白天 → 投票 → 放逐 → PK → 第二夜）。旧实现已备份到 `graphs/legacy/`。

## 实现记录

- 图定义：`graphs/assistant.json`（47 节点，入口 `load_game_state_file`，`host_router` 中央路由）；
- 规则脚本：`graphs/scripts/`（26 个 script 节点）；
- LLM 节点：7 个座位发言 + 8 个座位投票 + 狼队讨论/决策 + 女巫/预言家/猎人决策，prompts 在 `graphs/prompts/`；
- 部署调整：`deploy.yaml` 中 `max_iterations` 提高到 120；
- 上下文补全：所有 LLM 玩家统一携带“公开记录（死亡/票型/放逐/胜负）+ 私有记忆（`seat_memory`）+ 自己的查验/投票/发言历史”；
- 已知待办：
  - `scenarios/tests/werewolf/` 旧用例尚未按新规则重写；
  - 猎人开枪路径尚未端到端验证；
  - 每轮 LLM 调用较多，单回合耗时/成本偏高，后续可按“一个阶段一次用户输入”进一步拆分。

本文档记录 `examples/forge/scenarios/raids/werewolf` 的重构方案。目标是让 8 个玩家（7 个 LLM 电脑玩家 + 1 个人类玩家）真正参与一局狼人杀：LLM 负责决策和表达，脚本只负责规则校验与结算。

## 背景与动机

当前实现存在以下问题：

- 1 号、2 号发言是写死的脚本，只有 4~8 号是 LLM；
- 狼人刀谁、预言家查谁、女巫救不救/毒不毒全是固定优先级，女巫直接写死“不使用解药和毒药”；
- NPC 投票是确定性函数，跟公开焦点、跟用户、找不到狼就按顺序投；
- 用户固定是 3 号村民，没有夜间行动；
- 胜负只有“好人胜”一条路径，没有狼人屠边、猎人开枪、女巫用药这些真正的局势变化；
- 赛后复盘也是脚本台词。

核心思路：**LLM 负责决策和表达，脚本负责校验和结算**。规则节点只校验合法性，不限制策略选择（自刀、刀狼、自投、弃票等都是合法策略）。

## 定稿规则（按通用狼人杀规则）

| 规则点 | 定稿 |
|---|---|
| 狼人刀人 | 每夜全队统一一刀，目标必须存活；允许自刀和刀狼；狼人夜间互认，私密商量后决定 |
| 女巫 | 解药、毒药各一瓶，整局各用一次，每夜最多用一瓶；不可自救；解药未用时每夜能看到刀口，用掉解药后不再知道刀口；毒药可毒任意存活玩家 |
| 猎人 | 被狼刀或被放逐时翻牌开枪，带走一名存活玩家；被女巫毒死不能开枪 |
| 预言家 | 每夜查验一名其他存活玩家，只公布阵营（金水/查杀），不能验自己 |
| 投票 | 存活玩家每人一票；允许弃票；自投按经典规则允许（网杀普遍禁止属于房规，代码里留开关） |
| 平票 | 第一次平票时，平票玩家进 PK 台再发一轮言，其余存活玩家只能投 PK 台上的人；再平票则平安日，无人出局，直接进夜 |
| 胜利条件 | 好人胜：所有狼出局；狼人胜：屠边——神职（预言家、女巫、猎人）全灭或平民全灭；天亮结算时双方条件同时满足则“狼刀在先”，狼胜 |
| 遗言 | 首夜死亡和白天被放逐有遗言，其余无遗言 |

角色配置：8 人局，3 狼 + 预言家 + 女巫 + 猎人 + 2 平民，人类玩家随机身份。

## 整体流程

```
输入校验(指令? /reset → 归档+初始化)
   ↓ 自然语言
进度判断(未初始化→初始化; 否则按 phase/waiting_for 路由)
   ↓
主持人路由(纯脚本, 按状态选规则节点)
   ↓
[夜狼规则]→ 狼队私密讨论 → 交目标 → 校验 → 下个规则节点
[女巫规则]→ 私密给刀口 → 决定救/毒 → 校验 → 下个规则节点
[预言家规则]→ 私密查验 → 校验 → 下个规则节点
[天亮结算]→ 死亡/猎人开枪/胜负 → 主持人公布 → 日间规则
[日言规则]→ 轮流: NPC→玩家节点; 用户→提问+存盘+结束
[投票规则]→ 私密收票(含用户)→ 统一公布 → 平票则 PK
[PK规则]→ 平票者发言 → 再投票 → 平安日则进夜
[赛后规则]→ 基于归档数据复盘
```

## 节点清单

按现有引擎能力落地：规则逻辑是 script 节点，决策和发言是 inference 节点，主持人提问 + 存档结束是暂停点。

### 入口/控制层

| 节点 | 类型 | 职责 |
|---|---|---|
| `input_validate` | script | 判断 `/` 开头的指令；`/reset` 置重置标记，其他未知指令直接回复并结束 |
| `progress_route` | script | 未初始化或收到 `/reset` → `init_game`；否则按 `phase` / `waiting_for` 路由到 `host_router` |
| `init_game` | script | 归档旧局到 `archive/game_<id>.json`；洗牌发 8 张牌（3狼+预言家+女巫+猎人+2民，人类随机身份）；写入初始状态 |
| `host_opening` | script | 公开播报座位，私密告诉人类玩家身份 |
| `host_router` | script | 中央路由：按阶段选规则节点；只路由和调确定性结算，不做 LLM |
| `save_state` | script | 每个暂停点之后持久化 |

### 规则节点（script，纯确定性）

| 节点 | 职责 |
|---|---|
| `night_wolf_rule` | 组狼队私密频道；LLM 狼人讨论 → 指定狼人交目标；人类是狼时主持人私密提问并暂停；校验目标存活（允许自刀/刀狼） |
| `night_witch_rule` | 女巫已死或药已用完则跳过；否则私密告知刀口（解药未用时）；LLM 女巫决策或人类女巫提问暂停；校验每夜一瓶药、药量、不能自救、目标存活 |
| `night_seer_rule` | 预言家已死则跳过；私密查验；校验目标存活且不能验自己；结果只进私有记录 |
| `dawn_resolve` | 结算刀口（被救则不死）、毒药、死亡名单；猎人被刀/被放逐时触发开枪（人类则暂停提问，LLM 则决策），被毒不能开；判定胜负（狼刀在先） |
| `day_announce` | 公布死亡、存活名单、发言顺序 |
| `day_speech_rule` | 按发言顺序循环：LLM 玩家节点发言并公开；轮到人类则主持人公开提问、暂停 |
| `vote_rule` | 私密收集每个存活玩家的投票（LLM 逐个决策，人类提问暂停）；校验投存活玩家或弃票；收齐后统一公布 |
| `vote_tally` | 统计票型；平票则进 PK，否则执行放逐 |
| `pk_rule` | PK 台玩家各发言一轮，再私密投一轮（只能投 PK 台上的玩家）；再平票则平安日直接进夜 |
| `exile_resolve` | 执行放逐死亡、猎人开枪、胜负判定，进入下一夜或赛后 |
| `post_game_rule` | 基于归档数据复盘；不重开，除非 `/reset` |

### 玩家节点（inference，输出只回规则节点）

- `wolf_discuss`：每个狼人在私密频道讨论并提名目标
- `wolf_decide`：指定狼人输出最终目标
- `witch_decide` / `seer_decide` / `hunter_decide`：对应角色输出结构化决策
- `seat_N_speak`：7 个座位的日间发言
- `seat_N_vote` / `seat_N_pk_vote`：私密投票

校验内嵌在规则节点里，不单独拆节点；每个校验失败都带原因重试，LLM 玩家重试上限 2 次，超限按弃权/合法默认值处理。

## 状态字段

`game_state.json` 核心结构：

```jsonc
{
  "meta": { "game_id": "...", "seed": "...", "version": 1 },
  "phase": "night_wolf | night_witch | night_seer | dawn | day_speech | vote | pk_speech | pk_vote | ended",
  "waiting_for": "",  // wolf_target / witch_action / seer_action / hunter_shot / day_speech / human_vote / pk_speech / pk_vote
  "day": 1,
  "seats": [{ "id": 1, "name": "林知", "persona": "...", "role": "werewolf", "alive": true, "death_reason": "", "death_day": 0, "is_human": false }],
  "alive": [1,2,3,4,5,6,7,8],
  "player_seat": 5,
  "public_log": [],
  "public_focus": "",
  "speaker_order": [],     // 当天发言顺序
  "speech_index": 0,
  "night": {
    "wolf_target": 0, "wolf_decided": false,
    "witch_save": 0, "witch_poison": 0, "witch_decided": false,
    "seer_target": 0, "seer_decided": false
  },
  "witch": { "save_used": false, "poison_used": false },   // 解药未用 → 每夜知道刀口
  "seer_results": [{ "day": 1, "seer": 5, "target": 2, "camp": "好人阵营" }],
  "current_votes": [{ "voter": 1, "target": 3 }],          // 收齐前不公开
  "vote_records": [],
  "pk": { "candidates": [], "round": 0 },
  "hunter_shot_pending": 0,
  "winner": "",
  "last_night_kill": 0,
  "last_exile": 0,
  "seat_memory": { "1": "...", "2": "..." }                // 每个玩家私有记忆行
}
```

同时同步 board 变量方便图条件路由：`werewolf_phase`、`werewolf_waiting_for`、`seat_N_alive`、`human_is_wolf/witch/seer/hunter` 等。

## 信息组装契约

- 公有视图：阶段、天数、存活座位、公开日志、公开焦点、最近发言、历史票型、死亡信息；
- 私有视图：自己的身份、自己的查验结果、自己的记忆、自己之前发过的话；狼人额外看到队友；女巫在解药未用时看到刀口；
- 永不进入任何玩家上下文：其他玩家的身份、尚未公布的夜间行动、未公开的投票。

## 人类玩家交互协议

- 日间发言：主持人公开提问，用户输入作为公有信息进入主通道；
- 夜间行动/投票：主持人私密提问，用户的输入只进私有通道，不进入 LLM 玩家可读的公有历史；结算后由主持人决定公布什么；
- 暂停恢复：每个需要用户输入的点都通过 `waiting_for` 字段记录，主持人提问后 `save_state` 结束本轮，下一轮从输入校验进入图并按 `waiting_for` 精确恢复。

## 实现细节

- 现有脚本中 `seatByID`、`syncStateViews`、`publicStateView` 等辅助函数在每个脚本里重复粘贴。重构前先确认 JS runtime 是否支持加载共享脚本；支持则抽成公共库，否则继续按脚本复制，但保证改一处规则不会漏掉别处；
- 单个回合内 LLM 调用数量会明显增加，注意 `max_iterations` 上限与 token/延迟成本，必要时把一个“回合”拆成多次用户输入推进；
- 测试策略随之调整：不再断言具体角色死亡或具体票型，改为校验规则不破——非法动作被拒、每个阶段只推进一次、死亡与状态正确、胜负结算正确。
