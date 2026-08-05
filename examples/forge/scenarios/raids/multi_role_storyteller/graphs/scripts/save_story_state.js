// Persist durable board variables to the workspace so the
// next turn continues. Per-turn scratch variables are skipped.
var durable = {};
durable["story_state"] = board.getVar("story_state");
durable["blueprint_state"] = board.getVar("blueprint_state");
durable["cast_state"] = board.getVar("cast_state");
durable["user_state"] = board.getVar("user_state");
durable["continuity_state"] = board.getVar("continuity_state");
durable["ending_state"] = board.getVar("ending_state");
durable["internal_turn_count"] = board.getVar("internal_turn_count");
fs.write("story_state.json", JSON.stringify(durable));
