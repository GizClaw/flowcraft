const state = board.getVar("werewolf_game_state") || {};
function seatName(id) {
  const n = Number(id);
  for (const seat of state.seats || []) if (Number(seat.id) === n) return seat.name || "";
  return "";
}
const killed = Number(state.last_night_kill || 0);
const killedText = killed ? "昨夜" + killed + "号" + seatName(killed) + "死亡。" : "昨夜无人死亡。";
const alive = (state.alive || []).map(Number).sort(function(a, b) { return a - b; });
const aliveText = alive.map(function(id) { return id + "号" + seatName(id); }).join("、");
const first = alive.length ? alive[0] : 0;
const firstText = first ? "请" + first + "号" + seatName(first) + "开始发言。" : "";
const text = "现在是第" + (state.day || 1) + "天白天，" + killedText + "仍存活玩家按座位顺序发言：" + aliveText + "。" + firstText;
host.emit("token", { content: text });
