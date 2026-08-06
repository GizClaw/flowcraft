const state = board.getVar("werewolf_game_state") || {};
function isAlive(id) {
  const n = Number(id);
  return (state.alive || []).map(Number).indexOf(n) >= 0;
}
function seatRole(id) {
  const n = Number(id);
  for (const seat of state.seats || []) if (Number(seat.id) === n) return seat.role || "";
  return "";
}
function chooseSeerTarget() {
  const priority = [1, 5, 2, 4, 6, 7, 8];
  for (const id of priority) {
    if (id !== Number(state.seer_seat || 0) && isAlive(id) && seatRole(id) === "werewolf") return id;
  }
  return 0;
}
function firstLivingRole(role) {
  for (const seat of state.seats || []) {
    if (seat.alive === true && seat.role === role) return Number(seat.id);
  }
  return 0;
}
const seerSeat = Number(state.seer_seat || firstLivingRole("seer") || 0);
state.pending_seer_target = seerSeat > 0 && isAlive(seerSeat) ? (chooseSeerTarget() || firstLivingRole("werewolf")) : 0;
if (state.pending_seer_target > 0) {
  state.seer_results = Array.isArray(state.seer_results) ? state.seer_results : [];
  const exists = state.seer_results.some(function(r) {
    return Number(r.day) === Number(state.day) && Number(r.seer) === seerSeat && Number(r.target) === Number(state.pending_seer_target);
  });
  if (!exists) state.seer_results.push({
    day: state.day,
    seer: seerSeat,
    target: state.pending_seer_target,
    camp: seatRole(state.pending_seer_target) === "werewolf" ? "狼人阵营" : "好人阵营"
  });
}
state.last_event = "seer_night_action";
board.setVar("werewolf_game_state", state);
host.emit("token", { content: "预言家请睁眼，选择一名玩家查验身份。主持人给出查验结果。预言家请闭眼。" });
