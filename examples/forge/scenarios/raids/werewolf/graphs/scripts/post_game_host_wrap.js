const state = board.getVar("werewolf_game_state") || {};
const winner = state.winner === "good" ? "好人阵营" : (state.winner === "werewolf" ? "狼人阵营" : "本局");
const mode = String(board.getVar("post_game_mode") || "host_only");
let text = "这局已经结束，" + winner + "获胜。当前不会重新发牌或重开局，可以继续复盘刚才的发言、投票和身份安排。";
if (mode === "role_chain") text = "以上是各位玩家的赛后复盘。这局已经结束，" + winner + "获胜；后续只继续复盘本局，不重新发牌。";
host.emit("token", { content: text });
