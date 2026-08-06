function parseMove(line) {
  const text = String(line || "");
  const action = (text.match(/action=([^;\n]+)/) || [])[1] || "other";
  const target = (text.match(/target=([^;\n]+)/) || [])[1] || "";
  return { action: action.trim(), target: String(target || "").trim() };
}
function msgText(v) {
  if (typeof v === "string") return v;
  if (!v || typeof v !== "object") return "";
  const content = v.Content || v.content;
  if (content && typeof content.Text === "function") {
    const t = content.Text();
    if (typeof t === "string" && t !== "") return t;
  }
  const parts = Array.isArray(content && content.Parts) ? content.Parts : (content && Array.isArray(content.parts) ? content.parts : []);
  return parts.map(function(p) {
    return p && (p.type === "text" || p.Type === "text") && typeof (p.text || p.Text) === "string" ? (p.text || p.Text) : "";
  }).join("");
}
function parseFallback(text) {
  const raw = String(text || "");
  const seat = (raw.match(/([1-8])\s*号/) || raw.match(/([1-8])/ ) || [])[1] || "";
  if (/不对|怎么|为什么|为啥|是不是|吗|呢|记错|重复|谁死|现在/.test(raw) && !/我是预言家|我查验|我验|我昨晚|我夜里|我要查|我能查|我是女巫|我是猎人|我是狼人/.test(raw)) {
    return { action: "ask", target: seat };
  }
  if (/平民|村民|闭眼民/.test(raw) && /信|相信|支持|怀疑|倾向|先出|今天出|出掉|归票|跟票|继续跟|站边|表水|发言/.test(raw)) {
    return { action: "speech", target: seat };
  }
  if (/(阿澈|2号|二号).*(预言家|查验|报验|查杀)|((信|相信|支持|站边|跟).*(阿澈|2号|二号))/.test(raw)) {
    return { action: "speech", target: seat };
  }
  if (/(我要|我想|我现在|现在|本轮|轮到我|夜里|今晚|今夜).*(查验|验人|刀人|杀|毒|救|用药|开枪)/.test(raw) || /(查验|验人|刀人|毒|救|用药|开枪).*(可以吗|行吗|能不能|要不要)/.test(raw)) {
    return { action: "night_action", target: seat };
  }
  if (/投票|我投|放逐|出掉|归票|票/.test(raw)) return { action: "vote", target: seat };
  if (/为什么|规则|现在|谁死|身份/.test(raw)) return { action: "ask", target: seat };
  return { action: "speech", target: seat };
}
function addPublic(state, line) {
  const log = Array.isArray(state.public_log) ? state.public_log.slice() : [];
  log.push(line);
  state.public_log = log;
}
function roleLabel(role) {
  const names = { werewolf: "狼人", seer: "预言家", witch: "女巫", hunter: "猎人", villager: "平民" };
  return names[String(role || "")] || String(role || "");
}
function publicStateView(state) {
  const aliveSet = {};
  for (const id of state.alive || []) aliveSet[String(id)] = true;
  function seatName(id) {
    const n = Number(id);
    for (const seat of state.seats || []) if (Number(seat.id) === n) return seat.name || "";
    return "";
  }
  return {
    phase: state.phase || "setup",
    day: state.day || 0,
    player: { seat: state.player_seat || 3 },
    seats: (state.seats || []).map(function(s) {
      return { id: s.id, name: s.name, alive: aliveSet[String(s.id)] === true, speaking_style: s.persona || "" };
    }),
    public_log: Array.isArray(state.public_log) ? state.public_log : [],
    last_night_kill: state.last_night_kill || 0,
    last_exile: state.last_exile || 0,
    vote_retry_reason: state.vote_retry_reason || "",
    winner: state.winner || "",
    public_focus: state.public_focus || ""
  };
}
function syncStateViews(state) {
  const aliveSet = {};
  for (const id of state.alive || []) aliveSet[String(id)] = true;
  state.role_assignment = {
    seed: state.role_seed || "unassigned",
    player_seat: state.player_seat || 3,
    player_role: state.player_role || "villager",
    seats: (state.seats || []).map(function(s) { return { id: s.id, name: s.name, role: s.role, persona: s.persona }; })
  };
  state.phase_state = {
    started: state.started === true,
    phase: state.phase || "setup",
    day: state.day || 0,
    winner: state.winner || "",
    last_event: state.last_event || "",
    pending_tool_event: state.pending_tool_event || ""
  };
  state.seat_state = {
    alive: state.alive || [],
    dead: (state.seats || []).filter(function(s) { return !aliveSet[String(s.id)]; }).map(function(s) { return s.id; }),
    last_night_kill: state.last_night_kill || 0,
    last_exile: state.last_exile || 0
  };
  for (let i = 1; i <= 8; i++) board.setVar("seat_" + i + "_alive", aliveSet[String(i)] ? "true" : "false");
  board.setVar("werewolf_phase", state.phase || "setup");
  board.setVar("werewolf_pending_tool_event", state.pending_tool_event || "");
  board.setVar("werewolf_pending_tool_detail", state.pending_tool_detail || "");
  board.setVar("werewolf_game_state", state);
  const promptStateText = JSON.stringify(publicStateView(state), null, 2);
  board.setVar("werewolf_game_state_text", promptStateText);
  board.setVar("werewolf_public_focus", state.public_focus || "暂无");
  board.setVar("werewolf_player_role_label", roleLabel(state.player_role || ""));
  function ch(name, text) {
    board.setChannel(name, [{
      role: "user",
      content: { parts: [{ type: "text", text: text + "\n\n当前状态:\n" + promptStateText }] }
    }]);
  }
  function rawCh(name, text) {
    board.setChannel(name, [{
      role: "user",
      content: { parts: [{ type: "text", text: String(text || "") }] }
    }]);
  }
  function recentHistory(limit) {
    const msgs = board.channel(board.MAIN_CHANNEL) || [];
    const start = Math.max(0, msgs.length - Number(limit || 10));
    return msgs.slice(start).map(function(msg) {
      const role = msg.role || "";
      const parts = Array.isArray(msg.content.parts) ? msg.content.parts : [];
      let node = "";
      const text = parts.map(function(part) {
        if (!part) return "";
        if (part.type === "text/xml" && typeof part.text === "string") {
          const m = part.text.match(/node id="([^"]+)"/);
          if (m) node = m[1];
          return "";
        }
        return part.type === "text" && typeof part.text === "string" ? part.text : "";
      }).join("").trim();
      if (!text) return "";
      return (node ? node : role) + ": " + text.slice(0, 220);
    }).filter(Boolean).join("\n");
  }
  function seatByID(id) {
    const n = Number(id);
    for (const seat of state.seats || []) if (Number(seat.id) === n) return seat;
    return { id: n, name: String(n) + "号", role: "", alive: false, persona: "" };
  }
  function seatPublicBehavior(id) {
    const map = {
      4: "犹豫但会观察票型，倾向多听一轮。",
      5: "打圆场、转移焦点，削弱对自己的压力。",
      6: "谨慎分析公开发言和票型，不抢节奏。",
      7: "强势归票，抓住最清晰的公开矛盾推进投票。",
      8: "已出局时只在赛后旁观复盘。"
    };
    return map[Number(id)] || "根据公开信息发言。";
  }
  function seatPublicCtx(id, text) {
    const history = recentHistory(6) || "暂无";
    const memory = String(board.getVar("public_memory") || "暂无");
    const focus = String(board.getVar("werewolf_public_focus") || "暂无");
    const seat = seatByID(id);
    const view = "座位=" + id + "; 姓名=" + (seat.name || "") + "; 存活=" + (seat.alive === true ? "true" : "false") + "; 发言风格=" + (seat.persona || "") + "; 本轮公开任务=" + seatPublicBehavior(id);
    board.setVar("seat_" + id + "_public_view", view);
    return text + "\n\n当前公开焦点:\n" + focus + "\n\n公开角色视图:\n" + view + "\n\n公开记忆:\n" + memory + "\n\n近期历史:\n" + history;
  }
  ch("host_day_channel", "主持人白天公告。");
  ch("seat_4_channel", seatPublicCtx(4, "发言人=4号小满；用第一人称短话发言。"));
  ch("seat_5_channel", seatPublicCtx(5, "发言人=5号周岚；用第一人称短话发言。"));
  ch("seat_6_channel", seatPublicCtx(6, "发言人=6号陈医生；用第一人称短话发言。"));
  ch("seat_7_channel", seatPublicCtx(7, "发言人=7号老赵；用第一人称短话发言。"));
  ch("seat_8_channel", seatPublicCtx(8, "发言人=8号苏禾；用第一人称短话发言。"));
  ch("vote_prompt_channel", "主持人投票提示，要求3号玩家明确投票座位号。");
  ch("retry_channel", "主持人纠正当前动作。");
}
const state = board.getVar("werewolf_game_state") || {};
let move = parseMove(msgText(board.getVar("werewolf_move_text")));
const fallback = parseFallback(state.latest_user_text || "");
if (move.action === "claim_power") move = { action: "speech", target: move.target || fallback.target || "" };
if (fallback.action === "speech" && move.action === "night_action") move = fallback;
if (move.action === "other" || move.action === "ask" || (fallback.action !== "speech" && fallback.action !== move.action)) move = fallback;
state.last_action = move.action;
state.last_target = move.target;
state.pending_tool_event = "";
state.pending_tool_detail = "";
if (move.action === "night_action") {
  state.last_event = "invalid_user_role_action";
  state.speech_valid = false;
  state.phase = "user_speech";
  board.setVar("speech_valid", "false");
} else {
  state.last_event = "user_speech_ok";
  state.speech_valid = true;
  state.phase = "npc_tail";
  addPublic(state, "第" + state.day + "天3号玩家发言：" + String(state.latest_user_text || "").slice(0, 120));
  board.setVar("speech_valid", "true");
}
syncStateViews(state);
