const state = board.getVar("werewolf_game_state") || null;
const kind = String(board.getVar("werewolf_input_kind") || "natural");
const fresh = !state || state.started !== true;
if (kind === "command_reset") {
  board.setVar("werewolf_init", "true");
  board.setVar("werewolf_init_reason", "reset");
  board.setVar("werewolf_command_kind", "");
} else if (kind === "command_status" || kind === "command_other") {
  board.setVar("werewolf_init", "false");
  board.setVar("werewolf_init_reason", "");
  board.setVar("werewolf_command_kind", kind);
} else if (fresh) {
  board.setVar("werewolf_init", "true");
  board.setVar("werewolf_init_reason", "fresh");
  board.setVar("werewolf_command_kind", "");
} else {
  board.setVar("werewolf_init", "false");
  board.setVar("werewolf_init_reason", "");
  board.setVar("werewolf_command_kind", "");
}
