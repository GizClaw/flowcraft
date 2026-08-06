const state = board.getVar("werewolf_game_state") || {};
const pendingEvent = state.pending_tool_event || "none";
const pendingDetail = state.pending_tool_detail || "";
const alive = Array.isArray(state.alive) ? state.alive.join(",") : "";
const publicLog = Array.isArray(state.public_log) ? state.public_log.slice(-4).join(" | ") : "";
function seatByID(id) {
  const n = Number(id);
  for (const seat of state.seats || []) if (Number(seat.id) === n) return seat;
  return { id: n, name: String(n) + "号", role: "", alive: false, persona: "" };
}
function roleLabel(role) {
  const names = { werewolf: "狼人", seer: "预言家", witch: "女巫", hunter: "猎人", villager: "平民" };
  return names[String(role || "")] || String(role || "");
}
function lastLinesForSeat(id) {
  const n = Number(id);
  const name = seatByID(n).name || "";
  const log = Array.isArray(state.public_log) ? state.public_log : [];
  return log.filter(function(line) {
    return String(line || "").indexOf(String(n) + "号") >= 0 || (name && String(line || "").indexOf(name) >= 0);
  }).slice(-4).join(" | ");
}
function seerLineForSeat(id) {
  const n = Number(id);
  const hits = (Array.isArray(state.seer_results) ? state.seer_results : []).filter(function(r) {
    return Number(r.seer) === n || Number(r.target) === n;
  }).slice(-3);
  if (!hits.length) return "none";
  return hits.map(function(r) {
    return "day=" + r.day + ",seer=" + r.seer + ",target=" + r.target + ",camp=" + r.camp;
  }).join(" | ");
}
function seatMemoryLine(id) {
  const seat = seatByID(id);
  return "seat_" + id + "_memory: " +
    "seat=" + id +
    "; name=" + (seat.name || "") +
    "; role=" + roleLabel(seat.role || "") +
    "; alive=" + (seat.alive === true ? "true" : "false") +
    "; death_reason=" + (seat.death_reason || "none") +
    "; death_day=" + (seat.death_day || "none") +
    "; day=" + (state.day || 0) +
    "; phase=" + (state.phase || "") +
    "; last_exile=" + (state.last_exile || 0) +
    "; last_night_kill=" + (state.last_night_kill || 0) +
    "; seer_context=" + seerLineForSeat(id) +
    "; public_context=" + (lastLinesForSeat(id) || "none");
}
board.setVar("tmp_game_progress_line",
  "game_progress: phase=" + (state.phase || "") +
  "; day=" + (state.day || 0) +
  "; alive=" + alive +
  "; winner=" + (state.winner || "none"));
board.setVar("tmp_public_timeline_line", "public_timeline: " + (publicLog || "none"));
board.setVar("tmp_resolution_log_line",
  "resolution_log: event=" + pendingEvent +
  "; last_event=" + (state.last_event || "") +
  "; target=" + (state.last_target || "") +
  "; detail=" + pendingDetail);
for (const id of [1, 2, 3, 4, 5, 6, 7, 8]) {
  const line = seatMemoryLine(id);
  board.setVar("tmp_seat_" + id + "_memory_line", line);
  board.setVar("seat_" + id + "_memory", line);
}
state.pending_tool_event = "";
state.pending_tool_detail = "";
board.setVar("werewolf_game_state", state);
board.setVar("werewolf_game_state_text", JSON.stringify({
  phase: state.phase || "",
  day: state.day || 0,
  player: { seat: state.player_seat || 3 },
  public_log: Array.isArray(state.public_log) ? state.public_log : [],
  alive: Array.isArray(state.alive) ? state.alive : [],
  winner: state.winner || "",
  public_focus: state.public_focus || ""
}, null, 2));
board.setVar("werewolf_public_focus", state.public_focus || "暂无");
board.setVar("werewolf_pending_tool_event", "");
board.setVar("werewolf_pending_tool_detail", "");
board.setVar("werewolf_phase", state.phase || "");
