// Persist durable board variables to the workspace so the
// next turn continues. Per-turn scratch variables are skipped.
var durable = {};
var allVars = board.getVars() || {};
for (var key in allVars) {
  if (!Object.prototype.hasOwnProperty.call(allVars, key)) continue;
  if (key.indexOf("tmp_") === 0 || key.indexOf("memory_") === 0) continue;
  durable[key] = allVars[key];
}
fs.write("game_state.json", JSON.stringify(durable));
