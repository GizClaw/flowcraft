const state = board.getVar("werewolf_game_state") || {};
const role = String(board.getVar("werewolf_player_role_label") || "平民");
const action = String(state.last_action || "");
let reason = "这个动作不符合当前阶段。";
if (action === "night_action") reason = "当前是白天发言阶段，不能执行夜间技能。你可以在发言里跳身份、诈身份或质疑别人。";
const text = reason + "现在仍轮到3号你发言，请用" + role + "视角说明你信谁、怀疑谁，以及理由。";
host.emit("token", { content: text });
