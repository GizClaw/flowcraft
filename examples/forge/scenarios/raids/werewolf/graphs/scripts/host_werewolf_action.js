const state = board.getVar("werewolf_game_state") || {};
function seatRole(id) {
  const n = Number(id);
  for (const seat of state.seats || []) if (Number(seat.id) === n) return seat.role || "";
  return "";
}
function isAlive(id) {
  const n = Number(id);
  return (state.alive || []).map(Number).indexOf(n) >= 0;
}
function chooseNightKill() {
  if (Number(state.day || 1) === 1 && isAlive(8) && seatRole(8) !== "werewolf") return 8;
  const priority = [4, 6, 7, 2, 8];
  for (const id of priority) {
    if (id !== Number(state.player_seat || 3) && isAlive(id) && seatRole(id) !== "werewolf") return id;
  }
  return 0;
}
state.pending_night_kill = chooseNightKill();
state.last_event = "werewolf_night_action";
board.setVar("werewolf_game_state", state);
host.emit("token", { content: "狼人请睁眼，确认今晚袭击目标。狼人请闭眼。" });
