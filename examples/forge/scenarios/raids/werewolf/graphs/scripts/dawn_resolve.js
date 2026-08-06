function seatByID(state, id) {
  const n = Number(id);
  for (const seat of state.seats || []) if (Number(seat.id) === n) return seat;
  return { id: n, name: String(n) + "号", role: "", alive: false, persona: "" };
}
function seatName(state, id) { return seatByID(state, id).name || (String(id) + "号"); }
function seatRole(state, id) { return seatByID(state, id).role || ""; }
function isAlive(state, id) { return (state.alive || []).map(Number).indexOf(Number(id)) >= 0; }
function aliveSeats(state) { return (state.alive || []).map(Number).sort(function(a, b) { return a - b; }); }
function firstAliveRole(state, role) {
  for (const id of aliveSeats(state)) if (seatRole(state, id) === role) return id;
  return 0;
}
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
  state.phase = "day";
  state.waiting_for = "";
  state.speaker_order = aliveSeats(state);
  state.speech_index = 0;
  const alive = aliveSeats(state);
  const aliveText = alive.map(function(id) { return id + "号" + seatName(state, id); }).join("、");
  state.public_focus = "当前公开焦点：第" + state.day + "天白天，仍存活玩家按座位顺序发言：" + aliveText + "。";
  host.emit("token", { content: "天亮了。现在是第" + state.day + "天白天，仍存活玩家：" + aliveText + "。请" + alive[0] + "号" + seatName(state, alive[0]) + "开始发言。" });
}

const state = board.getVar("werewolf_game_state") || {};
const night = state.night || {};
const knife = Number(night.wolf_target || 0);
const saved = knife > 0 && Number(night.witch_save || 0) === knife;
const poison = Number(night.witch_poison || 0);
const deaths = [];
if (knife > 0 && !saved && isAlive(state, knife)) deaths.push({ seat: knife, reason: "night_kill" });
if (poison > 0 && isAlive(state, poison) && poison !== knife) deaths.push({ seat: poison, reason: "witch_poison" });
for (const d of deaths) markDead(state, d.seat, d.reason, state.day);
state.last_night_kill = knife > 0 && !saved ? knife : 0;
if (deaths.length) {
  const texts = deaths.map(function(d) { return d.seat + "号" + seatName(state, d.seat); });
  addPublic(state, "第" + state.day + "夜：夜晚结束，" + texts.join("、") + "死亡。");
  addPublicEvent(state, "第" + state.day + "夜：" + deaths.map(function(d) {
    return d.seat + "号" + seatName(state, d.seat) + (d.reason === "witch_poison" ? "被毒死" : "被狼刀死");
  }).join("、") + "。");
} else {
  addPublic(state, "第" + state.day + "夜：夜晚结束，无人死亡。");
  addPublicEvent(state, "第" + state.day + "夜：平安夜，无人死亡。");
}

let hunterId = 0;
for (const seat of state.seats || []) if (seat.role === "hunter") hunterId = Number(seat.id);
const hunterDeath = deaths.filter(function(d) { return d.seat === hunterId; })[0];
board.setVar("werewolf_hunter_step", "");
if (hunterDeath && hunterDeath.reason !== "witch_poison") {
  state.hunter_pending = hunterId;
  state.phase = "hunter";
  state.waiting_for = "";
  syncVars(state);
} else {
  finishResolution(state, "dawn");
  syncVars(state);
}
