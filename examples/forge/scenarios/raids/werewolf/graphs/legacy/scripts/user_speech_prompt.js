const role = String(board.getVar("werewolf_player_role_label") || "平民");
const text = "轮到3号你发言。请用" + role + "视角说明你信谁、怀疑谁，以及理由。";
host.emit("token", { content: text });
