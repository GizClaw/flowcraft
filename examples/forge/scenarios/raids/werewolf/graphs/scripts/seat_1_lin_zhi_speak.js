const state = board.getVar("werewolf_game_state") || {};
function seatName(id) {
  const n = Number(id);
  for (const seat of state.seats || []) if (Number(seat.id) === n) return seat.name || "";
  return "";
}
function seatRole(id) {
  const n = Number(id);
  for (const seat of state.seats || []) if (Number(seat.id) === n) return seat.role || "";
  return "";
}
function chooseSeerTarget(seerSeat) {
  const priority = [1, 5, 2, 4, 6, 7, 8];
  for (const id of priority) {
    if (id !== Number(seerSeat) && (state.alive || []).map(Number).indexOf(id) >= 0 && seatRole(id) === "werewolf") return id;
  }
  return 0;
}
function addPublic(line) {
  const log = Array.isArray(state.public_log) ? state.public_log.slice() : [];
  log.push(line);
  state.public_log = log;
}
const killed = Number(state.last_night_kill || 0);
const deathText = killed > 0 ? "昨晚死的是" + killed + "号" + seatName(killed) + "。" : "昨晚无人死亡。";
let text = "我1号林知先发言。" + deathText + "我这里没有额外信息，只能先表水：我是好人，第一轮先听后面每个人怎么说，再看票型。";
if (seatRole(1) === "seer") {
  const target = chooseSeerTarget(1);
  if (target > 0) {
    state.seer_results = Array.isArray(state.seer_results) ? state.seer_results : [];
    const result = { day: Number(state.day || 1), seer: 1, target: target, camp: seatRole(target) === "werewolf" ? "狼人阵营" : "好人阵营" };
    state.seer_results.push(result);
    state.public_focus = "当前公开焦点：1号林知公开查验" + target + "号" + seatName(target) + "为" + result.camp + "。";
    addPublic("第" + result.day + "天：1号林知公开报验，查验" + target + "号" + seatName(target) + "为" + result.camp + "。");
    text = "我1号林知，预言家。" + deathText + "昨夜我查验" + target + "号" + seatName(target) + "，结果是" + result.camp + "。";
  }
}
board.setVar("werewolf_game_state", state);
board.setVar("werewolf_public_focus", state.public_focus || "暂无");
host.emit("token", { content: text });
