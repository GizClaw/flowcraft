const kind = String(board.getVar("werewolf_command_kind") || "");
const state = board.getVar("werewolf_game_state") || null;
function seatName(id) {
  const n = Number(id);
  for (const seat of (state && state.seats) || []) if (Number(seat.id) === n) return seat.name || "";
  return String(n) + "号";
}
function aliveSeats(s) {
  return ((s && s.alive) || []).map(Number).sort(function(a, b) { return a - b; });
}
if (kind === "command_status" && state && state.started === true) {
  const alive = aliveSeats(state).map(function(id) { return id + "号" + seatName(id); }).join("、");
  const text = "第" + (state.day || 0) + "天，阶段：" + (state.phase || "setup") + "；存活玩家：" + alive + "；" + (state.winner ? "本局已结束，" + (state.winner === "good" ? "好人阵营" : "狼人阵营") + "获胜。" : "游戏进行中。");
  host.emit("token", { content: text });
} else if (kind === "command_status") {
  host.emit("token", { content: "当前还没有进行中的狼人杀对局。输入“开始狼人杀”或 /reset 开始新的一局。" });
} else {
  host.emit("token", { content: "未知指令。支持：/reset 重新开局、/status 查看状态。" });
}
