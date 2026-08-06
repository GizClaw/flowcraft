function msgText(msg) {
  if (!msg) return "";
  const content = msg.content || msg;
  if (typeof content === "string") return content;
  const parts = Array.isArray(content.parts) ? content.parts : [];
  return parts.map(function(p) {
    return p && (p.type === "text" || p.type === "text/xml") && typeof (p.text || "") === "string" ? (p.text || "") : "";
  }).join("").trim();
}
function lastAssistant(channel) {
  const msgs = board.channel(channel) || [];
  for (let i = msgs.length - 1; i >= 0; i--) {
    if (msgs[i] && msgs[i].role === "assistant") return msgs[i];
  }
  return null;
}
function seatByID(state, id) {
  const n = Number(id);
  for (const seat of state.seats || []) if (Number(seat.id) === n) return seat;
  return { id: n, name: String(n) + "号", role: "", alive: false, persona: "" };
}
function seatName(state, id) { return seatByID(state, id).name || (String(id) + "号"); }
function isAlive(state, id) { return (state.alive || []).map(Number).indexOf(Number(id)) >= 0; }
function aliveSeats(state) { return (state.alive || []).map(Number).sort(function(a, b) { return a - b; }); }
function addPublic(state, line) {
  const log = Array.isArray(state.public_log) ? state.public_log.slice() : [];
  log.push(line);
  state.public_log = log;
}
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
const seat = Number(board.getVar("werewolf_speaker_seat") || 0);
const last = seat > 0 ? lastAssistant("seat_" + seat + "_channel") : null;
const text = last ? msgText(last) : "";
if (seat > 0 && !text && board.getVar("werewolf_speech_retry") !== "true") {
  // LLM produced reasoning only; ask once for actual speech.
  board.setVar("werewolf_speech_retry", "true");
  const keep = [];
  for (const m of board.channel("seat_" + seat + "_channel") || []) {
    if (m && m.role === "user") keep.push(m);
  }
  keep.push({
    role: "user",
    content: { parts: [{ type: "text", text: "你刚才没有输出任何发言内容。请直接以第一人称输出你的发言，不要思考过程或解释。" }] }
  });
  board.setChannel("seat_" + seat + "_channel", keep);
  board.setVar("werewolf_speech_step", "retry");
  syncVars(state);
} else {
  const finalText = text || "我暂时没有更多信息，先听后面的发言。";
  if (last && text) {
    board.appendChannel(board.MAIN_CHANNEL, last);
  } else {
    host.emit("token", { content: "（" + (seatName(state, seat) || (seat + "号")) + "）" + finalText });
  }
  const arr = Array.isArray(state.public_speeches) ? state.public_speeches.slice() : [];
  arr.push({ day: state.day || 0, seat: seat, text: finalText.slice(0, 300) });
  state.public_speeches = arr;
  addPublic(state, (seatName(state, seat) || (seat + "号")) + "发言：" + finalText.slice(0, 120));
  state.speech_index = Number(state.speech_index || 0) + 1;
  board.setVar("werewolf_speech_retry", "false");
  board.setVar("werewolf_speaker_seat", "");
  board.setVar("werewolf_speech_step", "done");
  syncVars(state);
}
