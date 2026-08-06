const state = board.getVar("werewolf_game_state") || {};
state.phase = "vote";
state.last_event = "discussion_ready_to_vote";
board.setVar("werewolf_game_state", state);
board.setVar("werewolf_game_state_text", JSON.stringify({
  phase: state.phase || "vote",
  day: state.day || 0,
  player: { seat: state.player_seat || 3 },
  public_log: Array.isArray(state.public_log) ? state.public_log : [],
  alive: Array.isArray(state.alive) ? state.alive : [],
  last_night_kill: state.last_night_kill || 0,
  last_exile: state.last_exile || 0,
  public_focus: state.public_focus || ""
}, null, 2));
board.setVar("werewolf_public_focus", state.public_focus || "暂无");
board.setVar("werewolf_phase", "vote");
