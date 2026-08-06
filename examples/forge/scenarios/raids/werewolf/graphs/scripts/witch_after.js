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
function seatRole(state, id) { return seatByID(state, id).role || ""; }
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
  if (/action=none|不救不毒|不用药|放弃|过$/.test(raw)) return { action: "none", target: 0 };
  const save = /救|解药/.test(raw) && !/不救/.test(raw);
  const poison = /毒/.test(raw) && !/不毒/.test(raw);
  if (save && poison) return { action: "invalid", target: n };
  if (save) return { action: "save", target: n };
  if (poison) return { action: "poison", target: n };
  if (!n) return { action: "none", target: 0 };
  return { action: "invalid", target: n };
}

const state = board.getVar("werewolf_game_state") || {};
state.witch = state.witch || { save_used: false, poison_used: false };
const witchSeat = firstAliveRole(state, "witch");
const action = parseWitchAction(msgText(lastAssistant("witch_channel")));
let applied = null;
if (action.action === "save" && !state.witch.save_used && action.target > 0 && action.target !== witchSeat &&
    action.target === Number(state.night && state.night.wolf_target || 0) && isAlive(state, action.target)) {
  applied = { action: "save", target: action.target };
} else if (action.action === "poison" && !state.witch.poison_used && action.target > 0 && isAlive(state, action.target)) {
  applied = { action: "poison", target: action.target };
}
if (applied && applied.action === "save") { state.witch.save_used = true; state.night.witch_save = applied.target; }
if (applied && applied.action === "poison") { state.witch.poison_used = true; state.night.witch_poison = applied.target; }
state.phase = "night_seer";
state.waiting_for = "";
board.setVar("werewolf_witch_step", "done");
syncVars(state);
