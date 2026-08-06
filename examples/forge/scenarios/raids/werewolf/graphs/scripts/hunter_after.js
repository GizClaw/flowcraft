function msgText(msg) {
  if (!msg) return "";
  const content = msg.content || msg;
  if (typeof content === "string") return content;
  const parts = Array.isArray(content.parts) ? content.parts : [];
  return parts.map(function(p) {
    return p && (p.type === "text" || p.type === "text/xml") && typeof (p.text || "") === "string" ? (p.text || "") : "";
  }).join("").trim();
}
function lastAssistant(channel) {
  const msgs = board.channel(channel) || [];
  for (let i = msgs.length - 1; i >= 0; i--) {
    if (msgs[i] && msgs[i].role === "assistant") return msgs[i];
  }
  return null;
}
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
const hunterId = Number(state.hunter_pending || 0);
const raw = msgText(lastAssistant("hunter_channel"));
const m = raw.match(/([1-8])\s*号/) || raw.match(/target=([1-8])/) || raw.match(/([1-8])/);
const n = m ? Number(m[1]) : 0;
if (!/none|不|放弃/.test(raw) && /开|枪|带|杀/.test(raw) && n > 0 && isAlive(state, n)) {
  markDead(state, n, "hunter_shot", state.day);
  addPublic(state, "猎人" + hunterId + "号" + seatName(state, hunterId) + "开枪带走" + n + "号" + seatName(state, n) + "。");
  addPublicEvent(state, "猎人" + hunterId + "号" + seatName(state, hunterId) + "开枪带走" + n + "号" + seatName(state, n) + "。");
  host.emit("token", { content: hunterId + "号猎人开枪，带走" + n + "号" + seatName(state, n) + "。" });
} else {
  host.emit("token", { content: hunterId + "号猎人选择不开枪。" });
  addPublicEvent(state, "猎人" + hunterId + "号放弃开枪。");
}
state.hunter_pending = 0;
state.waiting_for = "";
finishResolution(state, "day");
board.setVar("werewolf_hunter_step", "done");
syncVars(state);
