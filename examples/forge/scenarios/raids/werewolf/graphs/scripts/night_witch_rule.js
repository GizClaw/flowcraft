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
function parseWitchAction(text) {
  const raw = String(text || "");
  const seat = (raw.match(/([1-8])\s*号/) || raw.match(/target=([1-8])/) || raw.match(/([1-8])/ ) || [])[1] || "";
  const n = Number(seat || 0);
  if (/不救不毒|不用药|什么都不|放弃|过$|都不|不救也不毒/.test(raw)) return { action: "none", target: 0 };
  const save = /救|解药/.test(raw) && !/不救/.test(raw);
  const poison = /毒/.test(raw) && !/不毒/.test(raw);
  if (save && poison) return { action: "invalid", target: n };
  if (save) return { action: "save", target: n };
  if (poison) return { action: "poison", target: n };
  if (/不救|不毒/.test(raw)) return { action: "none", target: 0 };
  if (!n) return { action: "none", target: 0 };
  return { action: "invalid", target: n };
}
function witchKnifeText(state, saveUsed) {
  if (saveUsed) return "解药已使用，你今晚不知道刀口。";
  const knife = Number(state.night && state.night.wolf_target || 0);
  if (knife > 0) return "狼人今晚袭击了" + knife + "号" + seatName(state, knife) + "。";
  return "今晚无人被袭击。";
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
const witchSeat = firstAliveRole(state, "witch");
state.witch = state.witch || { save_used: false, poison_used: false };
const saveUsed = state.witch.save_used === true;
const poisonUsed = state.witch.poison_used === true;

board.setVar("werewolf_witch_step", "");

if (witchSeat <= 0 || (saveUsed && poisonUsed)) {
  state.phase = "night_seer";
  state.waiting_for = "";
  board.setVar("werewolf_witch_step", "done");
  syncVars(state);
} else if (state.waiting_for === "witch_action") {
  const action = parseWitchAction(latestUserText());
  let valid = true;
  if (action.action === "save") {
    valid = !saveUsed && action.target > 0 && action.target !== witchSeat &&
      action.target === Number(state.night && state.night.wolf_target || 0) && isAlive(state, action.target);
  } else if (action.action === "poison") {
    valid = !poisonUsed && action.target > 0 && isAlive(state, action.target);
  } else if (action.action !== "none") {
    valid = false;
  }
  if (!valid) {
    state.waiting_for = "witch_action";
    host.emit("token", { content: "女巫请重新决定：救" + (state.night.wolf_target || "X") + "号 / 毒X号 / 不用药。注意解药只能救当晚被袭击的目标，不能自救。" });
    syncVars(state);
  } else {
    if (action.action === "save") { state.witch.save_used = true; state.night.witch_save = action.target; }
    if (action.action === "poison") { state.witch.poison_used = true; state.night.witch_poison = action.target; }
    state.waiting_for = "";
    state.phase = "night_seer";
    board.setVar("werewolf_witch_step", "done");
    syncVars(state);
  }
} else if (witchSeat === Number(state.player_seat)) {
  refreshSeatMemory(state);
  state.waiting_for = "witch_action";
  host.emit("token", { content: "女巫请睁眼（私密）。" + witchKnifeText(state, saveUsed) + "\n\n" + publicSummary(state) + "\n\n请决定：救X号 / 毒X号 / 不用药。" });
  syncVars(state);
} else {
  refreshSeatMemory(state);
  const view = "你是" + witchSeat + "号女巫。私密信息：" + witchKnifeText(state, saveUsed) +
    "解药可用=" + (!saveUsed) + "；毒药可用=" + (!poisonUsed) + "。\n\n" + publicSummary(state) +
    "\n\n你的私有记忆：\n" + (state.seat_memory[String(witchSeat)] || "暂无") +
    "\n\n请输出最后一行：action=save; target=<座号> 或 action=poison; target=<座号> 或 action=none。";
  board.setChannel("witch_channel", [{ role: "user", content: { parts: [{ type: "text", text: view }] } }]);
  board.setVar("werewolf_witch_step", "decide");
  syncVars(state);
}
