const state = board.getVar("werewolf_game_state") || {};
const reason = String(state.vote_retry_reason || "请明确投一个仍存活、且不是自己的座位号。");
host.emit("token", { content: reason + "例如可以说“我投5号”。" });
