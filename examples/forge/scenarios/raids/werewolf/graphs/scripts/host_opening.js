const state = board.getVar("werewolf_game_state") || {};
function seatName(id) {
  const n = Number(id);
  for (const seat of state.seats || []) if (Number(seat.id) === n) return seat.name || "";
  return "";
}
function roleLabel(role) {
  const names = { werewolf: "狼人", seer: "预言家", witch: "女巫", hunter: "猎人", villager: "平民" };
  return names[String(role || "")] || String(role || "");
}
const seats = (state.seats || []).map(function(s) {
  return String(s.id) + "号" + (s.name || "");
}).join("、");
const text = "本局是8人狼人杀，座位为：" + seats + "。你是" + (state.player_seat || 3) + "号，身份是" + roleLabel(state.player_role || "") + "。现在游戏开始，进入第" + (state.day || 1) + "夜。";
host.emit("token", { content: text });
