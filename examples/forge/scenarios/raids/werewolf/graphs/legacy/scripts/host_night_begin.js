const state = board.getVar("werewolf_game_state") || {};
state.phase = "night_open";
state.last_event = "night_begin";
board.setVar("werewolf_phase", "night_open");
board.setVar("werewolf_game_state", state);
const text = "天黑请闭眼。现在是第" + (state.day || 1) + "夜，所有玩家进入夜间阶段。";
host.emit("token", { content: text });
