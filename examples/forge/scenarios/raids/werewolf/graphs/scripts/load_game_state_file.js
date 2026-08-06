// Restore durable game state saved to the workspace.
var savedState = null;
try {
  savedState = JSON.parse(fs.read("game_state.json"));
} catch (e) {
  savedState = null;
}
if (savedState && typeof savedState === "object") {
  for (var key in savedState) {
    if (Object.prototype.hasOwnProperty.call(savedState, key)) {
      board.setVar(key, savedState[key]);
    }
  }
}

function isAlive(state, id) {
  return (state.alive || []).map(Number).indexOf(Number(id)) >= 0;
}
function aliveSeats(state) {
  return (state.alive || []).map(Number).sort(function(a, b) { return a - b; });
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
  for (var i = 1; i <= 8; i++) board.setVar("seat_" + i + "_alive", isAlive(state, i) ? "true" : "false");
  board.setVar("werewolf_game_state_text", JSON.stringify(publicView(state), null, 2));
}

var state = board.getVar("werewolf_game_state") || null;
if (state && typeof state === "object") {
  syncVars(state);
} else {
  board.setVar("werewolf_game_state", null);
  board.setVar("werewolf_phase", "");
  board.setVar("werewolf_waiting_for", "");
  board.setVar("werewolf_next_rule", "");
  board.setVar("werewolf_game_state_text", "");
}
