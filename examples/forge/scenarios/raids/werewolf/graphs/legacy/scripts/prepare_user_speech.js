const state = board.getVar("werewolf_game_state") || {};
state.phase = "user_speech";
board.setVar("werewolf_phase", "user_speech");
board.setVar("werewolf_game_state", state);
board.setVar("werewolf_game_state_text", JSON.stringify({
  phase: state.phase || "user_speech",
  day: state.day || 0,
  player: { seat: state.player_seat || 3 },
  public_log: Array.isArray(state.public_log) ? state.public_log : [],
  alive: Array.isArray(state.alive) ? state.alive : [],
  last_night_kill: state.last_night_kill || 0,
  last_exile: state.last_exile || 0
}, null, 2));
board.setChannel("user_prompt_channel", [{
  role: "user",
  content: { parts: [{
    type: "text",
    text: [
      "主持人提示3号玩家发言。",
      "现在轮到3号玩家发言，可以用平民视角说明信谁、怀疑谁、理由是什么。"
    ].join("\n"),
  }] }
}]);
