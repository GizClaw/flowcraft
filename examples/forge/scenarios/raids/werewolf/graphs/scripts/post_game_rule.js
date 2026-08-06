function latestUserText() {
  const msgs = board.channel(board.MAIN_CHANNEL) || [];
  for (let i = msgs.length - 1; i >= 0; i--) {
    const msg = msgs[i] || {};
    if (msg.role !== "user") continue;
    if (typeof msg.content === "string" && msg.content.trim()) return msg.content.trim();
    const parts = Array.isArray(msg.content && msg.content.parts) ? msg.content.parts : [];
    const text = parts.map(function(p) {
      return p && p.type === "text" && typeof p.text === "string" ? p.text : "";
    }).join("").trim();
    if (text) return text;
  }
  return "";
}
function roleLabel(role) {
  const names = { werewolf: "狼人", seer: "预言家", witch: "女巫", hunter: "猎人", villager: "平民" };
  return names[String(role || "")] || String(role || "");
}
function addPublic(state, line) {
  const log = Array.isArray(state.public_log) ? state.public_log.slice() : [];
  log.push(line);
  state.public_log = log;
}
function seatByID(state, id) {
  const n = Number(id);
  for (const seat of state.seats || []) if (Number(seat.id) === n) return seat;
  return { id: n, name: String(n) + "号", role: "", alive: false, persona: "" };
}
function isAlive(state, id) { return (state.alive || []).map(Number).indexOf(Number(id)) >= 0; }
function aliveSeats(state) { return (state.alive || []).map(Number).sort(function(a, b) { return a - b; }); }
function publicView(state) {
  return {
    phase: state.phase || "setup",
    day: state.day || 0,
    player: { seat: state.player_seat || 0 },
    alive: aliveSeats(state),
    winner: state.winner || "",
    public_focus: state.public_focus || "",
    public_log: Array.isArray(state.public_log) ? state.public_log.slice(-8) : []
  };
}
function syncVars(state) {
  board.setVar("werewolf_game_state", state);
  board.setVar("werewolf_phase", state.phase || "");
  board.setVar("werewolf_waiting_for", state.waiting_for || "");
  board.setVar("werewolf_next_rule", state.next_rule || "");
  for (let i = 1; i <= 8; i++) board.setVar("seat_" + i + "_alive", isAlive(state, i) ? "true" : "false");
  board.setVar("werewolf_game_state_text", JSON.stringify(publicView(state), null, 2));
}

const state = board.getVar("werewolf_game_state") || {};
if (state.post_game_announced !== true) {
  const label = state.winner === "good" ? "好人阵营" : "狼人阵营";
  host.emit("token", { content: "本局结束，" + label + "获胜。输入 /reset 可开始新的一局；也可以继续复盘本局。" });
  state.post_game_announced = true;
  const text = latestUserText();
  if (/大家|全员|每个人|所有人|按座位|一圈|复盘|都说/.test(text)) {
    const lines = (state.seats || []).map(function(s) {
      return s.id + "号" + (s.name || "") + "：身份=" + roleLabel(s.role || "") + "，" + (s.alive === true ? "还在场" : "已出局（" + (s.death_reason || "未知") + "）");
    });
    host.emit("token", { content: "赛后复盘：\n" + lines.join("\n") });
  }
  addPublic(state, "游戏结束：" + label + "获胜。");
}
syncVars(state);
