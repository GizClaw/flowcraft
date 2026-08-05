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