const state = board.getVar("werewolf_game_state") || {};
const focus = String(state.public_focus || board.getVar("werewolf_public_focus") || "当前公开焦点已进入投票阶段。").trim();
let target = "";
const patterns = [/查验([1-8])号/, /围绕([1-8])号/, /怀疑([1-8])号/, /归票([1-8])号/, /投([1-8])号/];
for (const pattern of patterns) {
  const match = focus.match(pattern);
  if (match) {
    target = match[1];
    break;
  }
}
const example = target ? "我投" + target + "号" : "我投某号";
const text = focus + "\n请3号玩家明确投票座位号，例如“" + example + "”。";
host.emit("token", { content: text });
