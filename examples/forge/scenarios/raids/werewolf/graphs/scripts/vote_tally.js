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

const state = board.getVar("werewolf_game_state") || {};
const votes = Array.isArray(state.current_votes) ? state.current_votes : [];
const isPk = !!(state.pk && state.pk.mode === "vote");
const lines = votes.map(function(v) {
  return v.voter + "号" + seatName(state, v.voter) + "投" + (v.target > 0 ? v.target + "号" + seatName(state, v.target) : "弃票");
});
const tallyText = "第" + state.day + "天投票：" + lines.join("；");
addPublic(state, tallyText);
addPublicEvent(state, tallyText.slice(0, 300));
host.emit("token", { content: tallyText + "。" });
state.vote_records = state.vote_records || [];
const rec = { day: state.day, votes: votes.slice(), exiled: 0 };
state.vote_records = state.vote_records.concat([rec]);

const counts = {};
for (const v of votes) {
  if (!v.target) continue;
  counts[String(v.target)] = (counts[String(v.target)] || 0) + 1;
}
let maxCount = 0;
let maxSeats = [];
for (const key of Object.keys(counts)) {
  const c = counts[key];
  if (c > maxCount) { maxCount = c; maxSeats = [Number(key)]; }
  else if (c === maxCount) maxSeats.push(Number(key));
}
const exiled = maxCount > 0 && maxSeats.length === 1 ? maxSeats[0] : 0;

if (!isPk && maxCount === 0) {
  addPublic(state, "无人获得有效投票，无人被放逐，进入夜晚。");
  addPublicEvent(state, "第" + state.day + "天无人获得有效投票，无人被放逐。");
  host.emit("token", { content: "无人获得有效投票，无人被放逐，进入夜晚。" });
  startNextNight(state);
  board.setVar("werewolf_vote_step", "done");
  syncVars(state);
} else if (isPk && !exiled) {
  addPublic(state, "PK再次平票，无人被放逐，进入夜晚。");
  addPublicEvent(state, "PK再次平票，无人被放逐。");
  host.emit("token", { content: "PK再次平票，本轮无人被放逐，进入夜晚。" });
  startNextNight(state);
  board.setVar("werewolf_vote_step", "done");
  syncVars(state);
} else if (!isPk && !exiled) {
  const tied = maxSeats.slice();
  state.pk = { mode: "speech", candidates: tied, round: Number((state.pk || {}).round || 0) + 1 };
  state.phase = "pk";
  state.speaker_order = tied;
  state.speech_index = 0;
  state.waiting_for = "";
  const tiedText = tied.map(function(id) { return id + "号" + seatName(state, id); }).join("、");
  addPublic(state, "投票平票，进入PK：候选人为" + tiedText + "。");
  addPublicEvent(state, "投票平票，进入PK：候选人为" + tiedText + "。");
  host.emit("token", { content: "平票！" + tiedText + " 进入PK，请候选人再发一轮言。" });
  board.setVar("werewolf_vote_step", "done");
  syncVars(state);
} else {
  rec.exiled = exiled;
  state.exile_target = exiled;
  state.phase = "exile";
  state.waiting_for = "";
  board.setVar("werewolf_vote_step", "done");
  syncVars(state);
}
