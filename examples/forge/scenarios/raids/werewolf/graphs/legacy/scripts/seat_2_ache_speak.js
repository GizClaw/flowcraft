const state = board.getVar("werewolf_game_state") || {};
function seatName(id) {
  const n = Number(id);
  for (const seat of state.seats || []) if (Number(seat.id) === n) return seat.name || "";
  return "";
}
function addPublic(line) {
  const log = Array.isArray(state.public_log) ? state.public_log.slice() : [];
  log.push(line);
  state.public_log = log;
}
function seatRole(id) {
  const n = Number(id);
  for (const seat of state.seats || []) if (Number(seat.id) === n) return seat.role || "";
  return "";
}
const day = Number(state.day || 1);
const role = seatRole(2);
const result = (Array.isArray(state.seer_results) ? state.seer_results : []).filter(function(r) {
  return Number(r.day) === day && Number(r.seer) === 2;
}).slice(-1)[0] || {};
function chooseSeerTarget(seerSeat) {
  const priority = [1, 5, 2, 4, 6, 7, 8];
  for (const id of priority) {
    if (id !== Number(seerSeat) && (state.alive || []).map(Number).indexOf(id) >= 0 && seatRole(id) === "werewolf") return id;
  }
  return 0;
}
let target = Number(result.target || 0);
let text = "我2号阿澈发言。昨夜已经公布死亡信息，我先根据1号发言和后续票型观察，不急着定论。";
let camp = result.camp || "";
if (role === "seer" && target <= 0) {
  target = chooseSeerTarget(2);
  camp = target > 0 && seatRole(target) === "werewolf" ? "狼人阵营" : "好人阵营";
  if (target > 0) {
    state.seer_results = Array.isArray(state.seer_results) ? state.seer_results : [];
    state.seer_results.push({ day: day, seer: 2, target: target, camp: camp });
  }
}
if (role === "seer" && target > 0) {
  state.public_focus = "当前公开焦点：2号阿澈公开查验" + target + "号" + seatName(target) + "为" + camp + "；第" + day + "天发言和投票主要围绕" + target + "号" + seatName(target) + "是否出局展开。";
  addPublic("第" + day + "天：2号阿澈公开报验，查验" + target + "号" + seatName(target) + "为" + camp + "。");
  text = "我2号阿澈，预言家。昨夜我查验" + target + "号" + seatName(target) + "，结果是" + camp + "。今天我建议先围绕" + target + "号发言和投票，大家重点看他的表水、反咬和跟票关系。";
}
board.setVar("werewolf_game_state", state);
board.setVar("werewolf_public_focus", state.public_focus || "暂无");
host.emit("token", { content: text });
