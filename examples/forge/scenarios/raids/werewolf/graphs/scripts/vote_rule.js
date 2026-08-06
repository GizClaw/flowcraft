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
function recentPublicTranscript(state, limit) {
  const arr = Array.isArray(state.public_speeches) ? state.public_speeches : [];
  const start = Math.max(0, arr.length - Number(limit || 20));
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
function parseVote(text) {
  const raw = String(text || "");
  if (/弃票|弃权|不投|放弃|过$|不投不投/.test(raw)) return 0;
  const m = raw.match(/投\s*([1-8])\s*号?/) ||
    raw.match(/票\s*([1-8])\s*号?/) ||
    raw.match(/出\s*([1-8])\s*号?/) ||
    raw.match(/放逐\s*([1-8])\s*号?/) ||
    raw.match(/vote=([1-8])/) ||
    raw.match(/([1-8])\s*号/);
  return m ? Number(m[1]) : 0;
}
function buildVoteView(state, voter, isPk) {
  refreshSeatMemory(state);
  const transcript = recentPublicTranscript(state, 20);
  const obj = seatByID(state, voter);
  const seerRows = (state.seer_results || []).filter(function(r) { return Number(r.seer) === Number(voter); });
  const seerText = seerRows.map(function(r) {
    return "第" + r.day + "夜查验：" + r.target + "号" + seatName(state, r.target) + "为" + r.camp;
  }).join(" | ") || "暂无";
  const candidatesText = ((state.pk && state.pk.candidates) || []).map(function(id) { return id + "号"; }).join("、");
  const restriction = isPk ? "本轮只能投票给PK台上的玩家：" + candidatesText + "。" : "可以投任意存活玩家，也可以弃票。";
  return "你是" + voter + "号" + seatName(state, voter) + "，身份=" + roleLabel(obj.role || "") + "。" +
    "\n你的查验=" + seerText + "。" +
    "\n" + restriction +
    "\n\n公开焦点：" + (state.public_focus || "暂无") +
    "\n\n" + publicSummary(state) +
    "\n\n你的私有记忆：\n" + (state.seat_memory[String(voter)] || "暂无") +
    "\n\n近期发言：\n" + transcript +
    "\n\n请私密输出你的投票，最后一行：vote=<座号|0>（0=弃票）。";
}

const state = board.getVar("werewolf_game_state") || {};
const isPk = !!(state.pk && state.pk.mode === "vote");
const candidates = (state.pk && state.pk.candidates) || [];
const voters = Array.isArray(state.vote_pending) && state.vote_pending.length ? state.vote_pending : aliveSeats(state);
const candidatesText = candidates.map(function(id) { return id + "号"; }).join("、");

board.setVar("werewolf_vote_step", "");
board.setVar("werewolf_vote_seat", "");

if (state.waiting_for === "vote" || state.waiting_for === "pk_vote") {
  const target = parseVote(latestUserText());
  const valid = isPk ? (target === 0 || candidates.indexOf(target) >= 0) : (target === 0 || isAlive(state, target));
  if (!valid) {
    state.waiting_for = isPk ? "pk_vote" : "vote";
    host.emit("token", { content: isPk ? "请投给PK台上的" + candidatesText + "，例如“我投5号”。" : "请明确投票座号或弃票，例如“我投5号”或“弃票”。" });
    syncVars(state);
  } else {
    state.current_votes = state.current_votes || [];
    state.current_votes = state.current_votes.concat([{ voter: Number(state.player_seat), target: target }]);
    state.vote_index = Number(state.vote_index || 0) + 1;
    state.waiting_for = "";
    syncVars(state);
  }
}

if (state.waiting_for === "vote" || state.waiting_for === "pk_vote") {
  // invalid human vote: stay paused
} else if (Number(state.vote_index || 0) >= voters.length) {
  state.phase = "tally";
  state.waiting_for = "";
  board.setVar("werewolf_vote_step", "done");
  syncVars(state);
} else {
  const voter = Number(voters[state.vote_index]);
  if (voter === Number(state.player_seat)) {
    state.waiting_for = isPk ? "pk_vote" : "vote";
    host.emit("token", { content: isPk ? "请私密投票，只能投PK台上的" + candidatesText + "，或弃票。" : "请私密投票，输入“我投X号”或“弃票”。" });
    syncVars(state);
  } else {
    const view = buildVoteView(state, voter, isPk);
    board.setChannel("seat_" + voter + "_vote_channel", [{ role: "user", content: { parts: [{ type: "text", text: view }] } }]);
    board.setVar("werewolf_vote_seat", String(voter));
    board.setVar("werewolf_vote_step", "decide");
    syncVars(state);
  }
}
