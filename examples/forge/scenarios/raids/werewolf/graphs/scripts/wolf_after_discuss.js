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
function parseTarget(text) {
  const raw = String(text || "");
  const m = raw.match(/([1-8])\s*号/) ||
    raw.match(/target=([1-8])/) ||
    raw.match(/proposal=([1-8])/) ||
    raw.match(/(?:刀|杀|袭击)[^\d]{0,4}([1-8])\s*号?/) ||
    raw.match(/([1-8])/);
  return m ? Number(m[1]) : 0;
}

const state = board.getVar("werewolf_game_state") || {};
const seat = Number(board.getVar("werewolf_speaker_seat") || 0);
const spoken = seat > 0 ? msgText(lastAssistant("wolf_team_channel")) : "";
let target = parseTarget(spoken);
if (target <= 0 || !isAlive(state, target)) {
  target = aliveSeats(state).filter(function(id) { return id !== seat; })[0] || 0;
}
state.night = state.night || {};
state.night.wolf_proposals = state.night.wolf_proposals || [];
state.night.wolf_discussion = state.night.wolf_discussion || [];
state.night.wolf_proposals = state.night.wolf_proposals.concat([{ seat: seat, target: target }]);
state.night.wolf_discussion = state.night.wolf_discussion.concat([{ seat: seat, text: (spoken || "（仅给出提案 " + target + " 号）").slice(0, 300) }]);
state.night.wolf_slot = Number(state.night.wolf_slot || 0) + 1;
state.wolf_speaker_seat = 0;
board.setVar("werewolf_wolf_step", "");
board.setVar("werewolf_speaker_seat", "");
syncVars(state);
