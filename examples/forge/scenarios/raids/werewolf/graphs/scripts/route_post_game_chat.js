const state = board.getVar("werewolf_game_state") || {};
const text = String(state.latest_user_text || "").trim();
if (/大家|全员|每个人|所有人|按座位|一圈|都说/.test(text)) {
  board.setVar("post_game_mode", "role_chain");
} else {
  board.setVar("post_game_mode", "host_only");
}
