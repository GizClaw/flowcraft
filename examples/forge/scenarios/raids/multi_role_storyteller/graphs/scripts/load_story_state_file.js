// Restore durable game state saved to the workspace.
var savedState = null;
try {
  savedState = JSON.parse(fs.read("story_state.json"));
} catch (e) {
  savedState = null;
}
if (savedState && typeof savedState === "object") {
  if (Object.prototype.hasOwnProperty.call(savedState, "story_state")) board.setVar("story_state", savedState["story_state"]);
  if (Object.prototype.hasOwnProperty.call(savedState, "blueprint_state")) board.setVar("blueprint_state", savedState["blueprint_state"]);
  if (Object.prototype.hasOwnProperty.call(savedState, "cast_state")) board.setVar("cast_state", savedState["cast_state"]);
  if (Object.prototype.hasOwnProperty.call(savedState, "user_state")) board.setVar("user_state", savedState["user_state"]);
  if (Object.prototype.hasOwnProperty.call(savedState, "continuity_state")) board.setVar("continuity_state", savedState["continuity_state"]);
  if (Object.prototype.hasOwnProperty.call(savedState, "ending_state")) board.setVar("ending_state", savedState["ending_state"]);
  if (Object.prototype.hasOwnProperty.call(savedState, "internal_turn_count")) board.setVar("internal_turn_count", savedState["internal_turn_count"]);
}