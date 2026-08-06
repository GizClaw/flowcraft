function latestUserText() {
  const msgs = board.channel(board.MAIN_CHANNEL) || [];
  for (let i = msgs.length - 1; i >= 0; i--) {
    const msg = msgs[i] || {};
    if (msg.role !== "user") continue;
    if (typeof msg.content === "string" && msg.content.trim()) return msg.content.trim();
    const parts = Array.isArray(msg.content && msg.content.parts) ? msg.content.parts : [];
    const text = parts.map(function(p) {
      return p && p.type === "text" && typeof p.text === "string" ? p.text : "";
    }).join("").trim();
    if (text) return text;
  }
  return "";
}
function seatByID(state, id) {
  const n = Number(id);
  for (const seat of state.seats || []) if (Number(seat.id) === n) return seat;
  return { id: n, name: String(n) + "号", role: "", alive: false, persona: "" };
}
function seatName(state, id) { return seatByID(state, id).name || (String(id) + "号"); }
function seatRole(state, id) { return seatByID(state, id).role || ""; }
function roleLabel(role) {
  const names = { werewolf: "狼人", seer: "预言家", witch: "女巫", hunter: "猎人", villager: "平民" };
  return names[String(role || "")] || String(role || "");
}
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
function parseHunterAction(text) {
  const raw = String(text || "");
  const m = raw.match(/([1-8])\s*号/) || raw.match(/target=([1-8])/) || raw.match(/([1-8])/);
  const n = m ? Number(m[1]) : 0;
  if (/不|放弃|算了|不开/.test(raw)) return { action: "none", target: 0 };
  if (/开|枪|带|杀/.test(raw) && n > 0) return { action: "shoot", target: n };
  return { action: "none", target: 0 };
}
function recentPublicEvents(state, limit) {
  const arr = Array.isArray(state.public_events) ? state.public_events : [];
  return arr.slice(-Number(limit || 15)).join("\n") || "暂无";
}
function refreshSeatMemory(state) {
  const mem = {};
  const events = Array.isArray(state.public_events) ? state.public_events : [];
  for (const seat of state.seats || []) {
    const id = Number(seat.id);
    const seerRows = (state.seer_results || []).filter(function(r) { return Number(r.seer) === id; });
    const myVotes = (state.vote_records || []).map(function(r) {
      return (r.votes || []).filter(function(v) { return Number(v.voter) === id; }).map(function(v) {
        return "第" + r.day + "天投" + (v.target > 0 ? v.target + "号" : "弃票");
      }).join("；");
    }).filter(Boolean).join(" | ");
    const mySpeeches = (state.public_speeches || []).filter(function(s) { return Number(s.seat) === id; }).slice(-4).map(function(s) { return s.text; }).join(" | ");
    const related = events.filter(function(e) {
      return String(e).indexOf(String(id) + "号") >= 0 || (seat.name && String(e).indexOf(seat.name) >= 0);
    }).slice(-2).join(" | ");
    mem[String(id)] = "seat=" + id + "; name=" + (seat.name || "") + "; role=" + roleLabel(seat.role || "") +
      "; alive=" + (seat.alive === true ? "true" : "false") + "; death_reason=" + (seat.death_reason || "none") +
      "; death_day=" + (seat.death_day || "none") + "; day=" + (state.day || 0) + "; phase=" + (state.phase || "") +
      "; my_seer=" + (seerRows.map(function(r) { return "第" + r.day + "夜查" + r.target + "号为" + r.camp; }).join("；") || "none") +
      "; my_votes=" + (myVotes || "none") + "; my_speeches=" + (mySpeeches || "none") +
      "; related_events=" + (related || "none");
  }
  state.seat_memory = mem;
}
function publicSummary(state) {
  return "公开记录（最近事件）：\n" + recentPublicEvents(state, 15);
}

const state = board.getVar("werewolf_game_state") || {};
const hunterId = Number(state.hunter_pending || 0);
board.setVar("werewolf_hunter_step", "");
if (hunterId <= 0) {
  finishResolution(state, "day");
  board.setVar("werewolf_hunter_step", "done");
  syncVars(state);
} else if (state.waiting_for === "hunter_shot") {
  const action = parseHunterAction(latestUserText());
  if (action.action === "shoot" && action.target > 0 && isAlive(state, action.target)) {
    markDead(state, action.target, "hunter_shot", state.day);
    addPublic(state, "猎人" + hunterId + "号" + seatName(state, hunterId) + "开枪带走" + action.target + "号" + seatName(state, action.target) + "。");
    addPublicEvent(state, "猎人" + hunterId + "号" + seatName(state, hunterId) + "开枪带走" + action.target + "号" + seatName(state, action.target) + "。");
    host.emit("token", { content: hunterId + "号猎人开枪，带走" + action.target + "号" + seatName(state, action.target) + "。" });
    state.hunter_pending = 0;
    state.waiting_for = "";
    finishResolution(state, "day");
    board.setVar("werewolf_hunter_step", "done");
    syncVars(state);
  } else if (action.action === "none") {
    host.emit("token", { content: hunterId + "号猎人选择不开枪。" });
    addPublicEvent(state, "猎人" + hunterId + "号放弃开枪。");
    state.hunter_pending = 0;
    state.waiting_for = "";
    finishResolution(state, "day");
    board.setVar("werewolf_hunter_step", "done");
    syncVars(state);
  } else {
    state.waiting_for = "hunter_shot";
    host.emit("token", { content: "猎人请重新决定：开枪带走X号，或放弃。" });
    syncVars(state);
  }
} else if (hunterId === Number(state.player_seat)) {
  refreshSeatMemory(state);
  state.waiting_for = "hunter_shot";
  host.emit("token", { content: "你是猎人，刚刚出局（私密）。\n\n" + publicSummary(state) + "\n\n你可以开枪带走一名存活玩家，或选择不开枪。例如“开枪5号”或“放弃”。" });
  syncVars(state);
} else {
  refreshSeatMemory(state);
  const view = "你是" + hunterId + "号猎人，刚刚出局。\n\n" + publicSummary(state) +
    "\n\n你的私有记忆：\n" + (state.seat_memory[String(hunterId)] || "暂无") +
    "\n\n你可以开枪带走一名存活玩家，也可以不开枪。最后一行输出：action=shoot; target=<座号> 或 action=none。";
  board.setChannel("hunter_channel", [{ role: "user", content: { parts: [{ type: "text", text: view }] } }]);
  board.setVar("werewolf_hunter_step", "decide");
  syncVars(state);
}
