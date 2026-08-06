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
function isAlive(state, id) { return (state.alive || []).map(Number).indexOf(Number(id)) >= 0; }
function aliveSeats(state) { return (state.alive || []).map(Number).sort(function(a, b) { return a - b; }); }
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
  if (/弃票|弃权|不投|放弃|过$/.test(raw)) return 0;
  const m = raw.match(/vote=([1-8])/) ||
    raw.match(/投\s*([1-8])\s*号?/) ||
    raw.match(/票\s*([1-8])\s*号?/) ||
    raw.match(/([1-8])\s*号/);
  return m ? Number(m[1]) : 0;
}

const state = board.getVar("werewolf_game_state") || {};
const voter = Number(board.getVar("werewolf_vote_seat") || 0);
const isPk = !!(state.pk && state.pk.mode === "vote");
let target = voter > 0 ? parseVote(msgText(lastAssistant("seat_" + voter + "_vote_channel"))) : 0;
if (isPk) {
  if (target !== 0 && ((state.pk.candidates || []).indexOf(target) < 0)) target = (state.pk.candidates || [])[0] || 0;
} else {
  if (target !== 0 && !isAlive(state, target)) target = 0;
}
state.current_votes = state.current_votes || [];
state.current_votes = state.current_votes.concat([{ voter: voter, target: target }]);
state.vote_index = Number(state.vote_index || 0) + 1;
board.setVar("werewolf_vote_seat", "");
board.setVar("werewolf_vote_step", "done");
syncVars(state);
