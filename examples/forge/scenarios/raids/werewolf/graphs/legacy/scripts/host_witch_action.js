const state = board.getVar("werewolf_game_state") || {};
const text = "女巫请睁眼。昨晚有人遇袭，是否使用解药？是否使用毒药？女巫本夜选择不使用毒药。女巫请闭眼。";
host.emit("token", { content: text });
