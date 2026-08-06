const state = board.getVar("werewolf_game_state") || {};
const text = String(state.vote_result_summary || "").trim();
if (text) {
  board.setVar("host_vote_result_text", text);
  host.emit("token", { content: text });
}
