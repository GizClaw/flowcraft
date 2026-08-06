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
function speakerLabel(state, seat) {
  const n = Number(seat);
  const name = seatName(state, n) || (n + "号");
  return n === Number(state.player_seat) ? n + "号（人类玩家）" : n + "号" + name;
}
function seatRole(state, id) { return seatByID(state, id).role || ""; }
function isAlive(state, id) { return (state.alive || []).map(Number).indexOf(Number(id)) >= 0; }
function aliveSeats(state) { return (state.alive || []).map(Number).sort(function(a, b) { return a - b; }); }
function roleLabel(role) {
  const names = { werewolf: "狼人", seer: "预言家", witch: "女巫", hunter: "猎人", villager: "平民" };
  return names[String(role || "")] || String(role || "");
}
function addPublic(state, line) {
  const log = Array.isArray(state.public_log) ? state.public_log.slice() : [];
  log.push(line);
  state.public_log = log;
}
function addPublicSpeech(state, seat, text) {
  const arr = Array.isArray(state.public_speeches) ? state.public_speeches.slice() : [];
  arr.push({ day: state.day || 0, seat: Number(seat), text: String(text || "").slice(0, 300) });
  state.public_speeches = arr;
  addPublic(state, (seatName(state, seat) || (seat + "号")) + "发言：" + String(text || "").slice(0, 120));
}
function recentPublicTranscript(state, limit) {
  const arr = Array.isArray(state.public_speeches) ? state.public_speeches : [];
  const start = Math.max(0, arr.length - Number(limit || 14));
  return arr.slice(start).map(function(s) {
    return speakerLabel(state, s.seat) + "：" + String(s.text || "");
  }).join("\n") || "暂无";
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
function buildSpeakerView(state, seat) {
  refreshSeatMemory(state);
  const transcript = recentPublicTranscript(state, 14);
  const obj = seatByID(state, seat);
  const seerRows = (state.seer_results || []).filter(function(r) { return Number(r.seer) === Number(seat); });
  const seerText = seerRows.map(function(r) {
    return "第" + r.day + "夜查验：" + r.target + "号" + seatName(state, r.target) + "为" + r.camp;
  }).join(" | ") || "暂无";
  return "发言人=" + seat + "号" + seatName(state, seat) + "；身份=" + roleLabel(obj.role || "") + "；存活=true；发言风格=" + (obj.persona || "") +
    "\n私有视角：你的身份=" + roleLabel(obj.role || "") + "；你的查验=" + seerText +
    "；只能使用自己的身份、自己的查验和公开信息，不得知道其他玩家的隐藏身份。" +
    "\n\n当前公开焦点：" + (state.public_focus || "暂无") +
    "\n\n" + publicSummary(state) +
    "\n\n你的私有记忆：\n" + (state.seat_memory[String(seat)] || "暂无") +
    "\n\n近期公开发言：\n" + transcript;
}

const state = board.getVar("werewolf_game_state") || {};
const candidates = (state.pk && state.pk.candidates) || [];
board.setVar("werewolf_speech_step", "");
board.setVar("werewolf_speaker_seat", "");
board.setVar("werewolf_speech_retry", "false");

if (state.waiting_for === "pk_speech") {
  addPublicSpeech(state, state.player_seat, latestUserText());
  state.speech_index = Number(state.speech_index || 0) + 1;
  state.waiting_for = "";
}
const order = Array.isArray(state.speaker_order) && state.speaker_order.length ? state.speaker_order : candidates;
if (Number(state.speech_index || 0) >= order.length) {
  state.pk.mode = "vote";
  state.phase = "vote";
  state.vote_pending = aliveSeats(state).filter(function(id) { return candidates.indexOf(id) < 0; });
  state.vote_index = 0;
  state.current_votes = [];
  state.waiting_for = "";
  board.setVar("werewolf_speech_step", "done");
  syncVars(state);
} else {
  const seat = Number(order[state.speech_index]);
  if (seat === Number(state.player_seat)) {
    state.waiting_for = "pk_speech";
    host.emit("token", { content: "你进入了PK台。请再发一轮言为自己拉票。" });
    syncVars(state);
  } else {
    const view = buildSpeakerView(state, seat) + "\n\n当前是PK发言阶段，请尽量为自己争取票数。";
    board.setChannel("seat_" + seat + "_channel", [{ role: "user", content: { parts: [{ type: "text", text: view }] } }]);
    board.setVar("werewolf_speaker_seat", String(seat));
    board.setVar("werewolf_speech_step", "speak");
    syncVars(state);
  }
}
