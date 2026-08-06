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
  const m = raw.match(/([1-8])\s*号/) ||
    raw.match(/target=([1-8])/) ||
    raw.match(/proposal=([1-8])/) ||
    raw.match(/(?:刀|杀|袭击|毒|救|验)[^\d]{0,4}([1-8])\s*号?/) ||
    raw.match(/([1-8])/);
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

const state = board.getVar("werewolf_game_state") || {};
if (!state.night || state.night.started !== true) {
  state.night = {
    started: true, wolf_slot: 0, wolf_proposals: [], wolf_discussion: [], wolf_decided: false,
    wolf_target: 0, witch_save: 0, witch_poison: 0, witch_decided: false,
    seer_target: 0, seer_decided: false
  };
  addPublic(state, "第" + (state.day || 1) + "夜：狼人请睁眼。");
}
const wolves = aliveSeats(state).filter(function(id) { return seatRole(state, id) === "werewolf"; });
const humanIsWolf = Number(state.player_seat) > 0 && seatRole(state, state.player_seat) === "werewolf" && isAlive(state, state.player_seat);
const consume = state.waiting_for === "wolf_target";

board.setVar("werewolf_wolf_step", "");
board.setVar("werewolf_speaker_seat", "");

if (consume) {
  const target = parseTarget(latestUserText());
  if (target <= 0 || !isAlive(state, target)) {
    state.waiting_for = "wolf_target";
    host.emit("token", { content: "狼人请重新确认要袭击的座号，例如“刀5号”。" });
    syncVars(state);
  } else {
    state.night.wolf_proposals = state.night.wolf_proposals || [];
    state.night.wolf_proposals = state.night.wolf_proposals.concat([{ seat: Number(state.player_seat), target: target }]);
    state.night.wolf_slot = Number(state.night.wolf_slot || 0) + 1;
    state.waiting_for = "";
    syncVars(state);
  }
}

if (state.waiting_for === "wolf_target") {
  // invalid human reply: stay paused
} else if (state.night.wolf_slot < wolves.length) {
  const seat = wolves[state.night.wolf_slot];
  if (seat === Number(state.player_seat)) {
    const mates = wolves.filter(function(id) { return id !== seat; }).map(function(id) { return id + "号" + seatName(state, id); }).join("、");
    refreshSeatMemory(state);
    state.waiting_for = "wolf_target";
    host.emit("token", { content: "狼人请睁眼（私密）。你的狼队友：" + mates + "。\n\n" + publicSummary(state) + "\n\n请选择今晚要袭击的座号，例如“刀5号”。" });
    syncVars(state);
  } else {
    const mates = wolves.filter(function(id) { return id !== seat; }).map(function(id) { return id + "号" + seatName(state, id); }).join("、");
    const discussion = (state.night.wolf_discussion || []).map(function(d) {
      return d.seat + "号" + seatName(state, d.seat) + "：" + d.text;
    }).join("\n") || "暂无";
    state.wolf_speaker_seat = seat;
    refreshSeatMemory(state);
    const view = "你是" + seat + "号" + seatName(state, seat) + "，身份：狼人。你的狼队友：" + mates +
      "。\n\n" + publicSummary(state) +
      "\n\n你的私有记忆：\n" + (state.seat_memory[String(seat)] || "暂无") +
      "。\n\n狼队讨论记录：\n" + discussion +
      "\n\n今晚请简短发言并给出建议，最后一行写 proposal=<座号>。";
    board.setChannel("wolf_team_channel", [{ role: "user", content: { parts: [{ type: "text", text: view }] } }]);
    board.setVar("werewolf_wolf_step", "discuss");
    syncVars(state);
  }
} else if (humanIsWolf) {
  const prop = (state.night.wolf_proposals || []).filter(function(p) { return Number(p.seat) === Number(state.player_seat); })[0];
  state.night.wolf_target = prop ? Number(prop.target) : 0;
  state.night.wolf_decided = true;
  state.phase = "night_witch";
  state.waiting_for = "";
  board.setVar("werewolf_wolf_step", "done");
  syncVars(state);
} else {
  const decider = wolves[0] || 0;
  board.setVar("werewolf_decider_seat", String(decider));
  const discussion = (state.night.wolf_discussion || []).map(function(d) {
    return d.seat + "号" + seatName(state, d.seat) + "：" + d.text;
  }).join("\n") || "暂无";
  refreshSeatMemory(state);
  board.setChannel("wolf_team_channel", [{
    role: "user",
    content: { parts: [{ type: "text", text: "你是狼队最终决策者（" + decider + "号）。\n\n" + publicSummary(state) + "\n\n你的私有记忆：\n" + (state.seat_memory[String(decider)] || "暂无") + "\n\n狼队讨论记录：\n" + discussion + "\n\n请输出今晚的最终决定，最后一行写 target=<座号>。" }] }
  }]);
  board.setVar("werewolf_wolf_step", "decide");
  syncVars(state);
}
