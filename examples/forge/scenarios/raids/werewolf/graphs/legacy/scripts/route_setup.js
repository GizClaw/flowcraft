const state = board.getVar("werewolf_game_state") || {};
if (state.phase === "ended") {
  board.setVar("werewolf_route", "post_game_free_chat");
} else if (state.started !== true || state.phase === "setup") {
  board.setVar("werewolf_route", "setup_game");
} else if (state.phase === "night_open") {
  board.setVar("werewolf_route", "night_open");
} else if (state.phase === "day_open") {
  board.setVar("werewolf_route", "day_open");
} else if (state.phase === "user_speech") {
  board.setVar("werewolf_route", "user_speech");
} else if (state.phase === "vote") {
  board.setVar("werewolf_route", "vote");
} else {
  board.setVar("werewolf_route", "day_open");
}
