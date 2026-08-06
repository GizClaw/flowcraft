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
function firstAliveRole(state, role) {
  for (const id of aliveSeats(state)) if (seatRole(state, id) === role) return id;
  return 0;
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
function parseTarget(text) {
  const raw = String(text || "");
  const m = raw.match(/([1-8])\s*号/) || raw.match(/target=([1-8])/) || raw.match(/([1-8])/);
  return m ? Number(m[1]) : 0;
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
function seerHistoryText(state, seerSeat) {
  const rows = (state.seer_results || []).filter(function(r) { return Number(r.seer) === Number(seerSeat); });
  return rows.map(function(r) { return "第" + r.day + "夜查" + r.target + "号" + seatName(state, r.target) + "为" + r.camp; }).join("；") || "暂无";
}

const state = board.getVar("werewolf_game_state") || {};
const seerSeat = firstAliveRole(state, "seer");
board.setVar("werewolf_seer_step", "");

if (seerSeat <= 0) {
  state.phase = "dawn";
  state.waiting_for = "";
  board.setVar("werewolf_seer_step", "done");
  syncVars(state);
} else if (state.waiting_for === "seer_action") {
  const target = parseTarget(latestUserText());
  if (target <= 0 || target === Number(state.player_seat) || !isAlive(state, target)) {
    state.waiting_for = "seer_action";
    host.emit("token", { content: "预言家请重新选择查验目标：输入“验X号”，目标必须存活且不能是自己。" });
    syncVars(state);
  } else {
    state.seer_results = state.seer_results || [];
    state.seer_results = state.seer_results.concat([{
      day: state.day || 1,
      seer: seerSeat,
      target: target,
      camp: seatRole(state, target) === "werewolf" ? "狼人阵营" : "好人阵营"
    }]);
    state.waiting_for = "";
    state.phase = "dawn";
    board.setVar("werewolf_seer_step", "done");
    syncVars(state);
  }
} else if (seerSeat === Number(state.player_seat)) {
  refreshSeatMemory(state);
  state.waiting_for = "seer_action";
  host.emit("token", { content: "预言家请睁眼（私密）。你的历史查验：" + seerHistoryText(state, seerSeat) + "。\n\n" + publicSummary(state) + "\n\n请选择今晚查验的目标，例如“验5号”。" });
  syncVars(state);
} else {
  refreshSeatMemory(state);
  const view = "你是" + seerSeat + "号预言家。你的历史查验：" + seerHistoryText(state, seerSeat) + "。\n\n" + publicSummary(state) +
    "\n\n你的私有记忆：\n" + (state.seat_memory[String(seerSeat)] || "暂无") +
    "\n\n请选择今晚查验的目标（必须存活、不能查验自己、避免重复查验）。最后一行输出：target=<座号>。";
  board.setChannel("seer_channel", [{ role: "user", content: { parts: [{ type: "text", text: view }] } }]);
  board.setVar("werewolf_seer_step", "decide");
  syncVars(state);
}
