const state = board.getVar("werewolf_game_state") || {};
function roleLabel(role) {
  const names = { werewolf: "狼人", seer: "预言家", witch: "女巫", hunter: "猎人", villager: "平民" };
  return names[String(role || "")] || String(role || "未知");
}
function seatByID(id) {
  for (const seat of state.seats || []) if (Number(seat.id) === Number(id)) return seat;
  return { id: id, name: String(id) + "号", role: "", alive: false };
}
function recentPublicLine(id) {
  const name = seatByID(id).name || "";
  const lines = Array.isArray(state.public_log) ? state.public_log : [];
  for (let i = lines.length - 1; i >= 0; i--) {
    const line = String(lines[i] || "");
    if (line.indexOf(String(id) + "号") >= 0 || (name && line.indexOf(name) >= 0)) return line;
  }
  return "暂无单独记录。";
}
const seat = seatByID(4);
const winner = state.winner === "good" ? "好人阵营" : (state.winner === "werewolf" ? "狼人阵营" : "本局");
const status = seat.alive === true ? "还在场" : "已出局";
const text = "我4号" + (seat.name || "") + "复盘：我的身份是" + roleLabel(seat.role) + "，当前" + status + "。最近和我有关的公开记录是：" + recentPublicLine(4) + " 这局结果是" + winner + "获胜。";
host.emit("token", { content: text });
