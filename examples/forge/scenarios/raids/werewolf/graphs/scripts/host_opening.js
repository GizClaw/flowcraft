const state = board.getVar("werewolf_game_state") || {};
function seatName(id) {
  const n = Number(id);
  for (const seat of state.seats || []) if (Number(seat.id) === n) return seat.name || "";
  return String(n) + "号";
}
function roleLabel(role) {
  const names = { werewolf: "狼人", seer: "预言家", witch: "女巫", hunter: "猎人", villager: "平民" };
  return names[String(role || "")] || String(role || "");
}
const seatsText = (state.seats || []).map(function(s) { return s.id + "号" + (s.name || ""); }).join("、");
host.emit("token", {
  content: "本局是8人狼人杀，座位为：" + seatsText + "。你是" + state.player_seat + "号，身份是" + roleLabel(state.player_role || "") + "（身份请保密）。现在游戏开始，进入第1夜。"
});
const log = Array.isArray(state.public_log) ? state.public_log.slice() : [];
log.push("游戏开始。");
state.public_log = log;

function isAlive(s, id) { return (s.alive || []).map(Number).indexOf(Number(id)) >= 0; }
function aliveSeats(s) { return (s.alive || []).map(Number).sort(function(a, b) { return a - b; }); }
function publicView(s) {
  return {
    phase: s.phase || "setup",
    day: s.day || 0,
    player: { seat: s.player_seat || 0 },
    alive: aliveSeats(s),
    winner: s.winner || "",
    public_focus: s.public_focus || "",
    public_log: Array.isArray(s.public_log) ? s.public_log.slice(-8) : []
  };
}
function syncVars(s) {
  board.setVar("werewolf_game_state", s);
  board.setVar("werewolf_phase", s.phase || "");
  board.setVar("werewolf_waiting_for", s.waiting_for || "");
  board.setVar("werewolf_next_rule", s.next_rule || "");
  for (let i = 1; i <= 8; i++) board.setVar("seat_" + i + "_alive", isAlive(s, i) ? "true" : "false");
  board.setVar("werewolf_game_state_text", JSON.stringify(publicView(s), null, 2));
}
syncVars(state);
