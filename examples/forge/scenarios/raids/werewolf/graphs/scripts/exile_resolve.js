function seatByID(state, id) {
  const n = Number(id);
  for (const seat of state.seats || []) if (Number(seat.id) === n) return seat;
  return { id: n, name: String(n) + "号", role: "", alive: false, persona: "" };
}
function seatName(state, id) { return seatByID(state, id).name || (String(id) + "号"); }
function seatRole(state, id) { return seatByID(state, id).role || ""; }
function isAlive(state, id) { return (state.alive || []).map(Number).indexOf(Number(id)) >= 0; }
function aliveSeats(state) { return (state.alive || []).map(Number).sort(function(a, b) { return a - b; }); }
function addPublic(state, line) {
  const log = Array.isArray(state.public_log) ? state.public_log.slice() : [];
  log.push(line);
  state.public_log = log;
}
function addPublicEvent(state, line) {
  const arr = Array.isArray(state.public_events) ? state.public_events.slice() : [];
  arr.push(String(line || ""));
  state.public_events = arr;
}
function markDead(state, id, reason, day) {
  const n = Number(id);
  if (!isAlive(state, n)) return;
  state.alive = (state.alive || []).filter(function(x) { return Number(x) !== n; });
  const seat = seatByID(state, n);
  seat.alive = false;
  seat.death_reason = String(reason || "dead");
  seat.death_day = Number(day || state.day || 0);
}
function allWolvesDead(state) {
  return aliveSeats(state).every(function(id) { return seatRole(state, id) !== "werewolf"; });
}
function wolfWinCond(state) {
  let gods = 0, villagers = 0;
  for (const id of aliveSeats(state)) {
    const r = seatRole(state, id);
    if (r === "seer" || r === "witch" || r === "hunter") gods++;
    if (r === "villager") villagers++;
  }
  return gods === 0 || villagers === 0;
}
function publicView(state) {
  return {
    phase: state.phase || "setup",
    day: state.day || 0,
    player: { seat: state.player_seat || 0 },
    alive: aliveSeats(state),
    winner: state.winner || "",
    public_focus: state.public_focus || "",
    public_log: Array.isArray(state.public_log) ? state.public_log.slice(-8) : []
  };
}
function syncVars(state) {
  board.setVar("werewolf_game_state", state);
  board.setVar("werewolf_phase", state.phase || "");
  board.setVar("werewolf_waiting_for", state.waiting_for || "");
  board.setVar("werewolf_next_rule", state.next_rule || "");
  for (let i = 1; i <= 8; i++) board.setVar("seat_" + i + "_alive", isAlive(state, i) ? "true" : "false");
  board.setVar("werewolf_game_state_text", JSON.stringify(publicView(state), null, 2));
}
function announceWinner(state) {
  const label = state.winner === "good" ? "好人阵营" : "狼人阵营";
  addPublic(state, label + "获胜。");
  state.public_focus = "当前公开焦点：" + label + "获胜，游戏结束。";
  host.emit("token", { content: "本局结束，" + label + "获胜。" });
}
function startNextNight(state) {
  state.day = Number(state.day || 1) + 1;
  state.phase = "night_wolf";
  state.waiting_for = "";
  state.night = {
    started: false, wolf_slot: 0, wolf_proposals: [], wolf_discussion: [], wolf_decided: false,
    wolf_target: 0, witch_save: 0, witch_poison: 0, witch_decided: false,
    seer_target: 0, seer_decided: false
  };
  state.current_votes = [];
  state.exile_target = 0;
  state.last_night_kill = 0;
  state.pk = { candidates: [], mode: "", round: 0 };
  state.public_focus = "当前公开焦点：进入第" + state.day + "夜。";
  addPublicEvent(state, "进入第" + state.day + "夜。");
  host.emit("token", { content: "天黑请闭眼，进入第" + state.day + "夜。" });
}
function finishResolution(state, order) {
  const goodWin = allWolvesDead(state);
  const wolfWin = wolfWinCond(state);
  let winner = "";
  if (order === "dawn") {
    if (wolfWin) winner = "werewolf";
    else if (goodWin) winner = "good";
  } else {
    if (goodWin) winner = "good";
    else if (wolfWin) winner = "werewolf";
  }
  if (winner) {
    state.winner = winner;
    state.phase = "ended";
    state.waiting_for = "";
    addPublicEvent(state, "游戏结束：" + (winner === "good" ? "好人阵营" : "狼人阵营") + "获胜。");
    announceWinner(state);
    return;
  }
  startNextNight(state);
}

const state = board.getVar("werewolf_game_state") || {};
const target = Number(state.exile_target || 0);
let hunterId = 0;
for (const seat of state.seats || []) if (seat.role === "hunter") hunterId = Number(seat.id);
board.setVar("werewolf_hunter_step", "");
if (target > 0 && isAlive(state, target)) {
  markDead(state, target, "exile", state.day);
  state.last_exile = target;
  addPublic(state, "第" + state.day + "天：" + target + "号" + seatName(state, target) + "被放逐。");
  addPublicEvent(state, "第" + state.day + "天：" + target + "号" + seatName(state, target) + "被放逐。");
  host.emit("token", { content: target + "号" + seatName(state, target) + "被放逐。" });
}
if (target === hunterId) {
  state.hunter_pending = hunterId;
  state.phase = "hunter";
  state.waiting_for = "";
  syncVars(state);
} else {
  finishResolution(state, "day");
  board.setVar("werewolf_hunter_step", "done");
  syncVars(state);
}
