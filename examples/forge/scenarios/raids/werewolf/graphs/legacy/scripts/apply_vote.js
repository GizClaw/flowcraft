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
function parseVote(text) {
  const raw = String(text || "");
  const seat = (raw.match(/([1-8])\s*号/) || raw.match(/([1-8])/ ) || [])[1] || "";
  if (/凭啥|为什么|为啥|不改|不想|不投|必须|要我|让我|规则|不对|怎么/.test(raw) && !/^\s*(我投|我投票|投票|我票|我出|我放逐)/.test(raw)) {
    return { action: "other", target: seat };
  }
  if (seat && (
    /^\s*(我\s*)?(投票|投|票|出|出掉|放逐)\s*[1-8]\s*号?/.test(raw) ||
    /我\s*(决定|就|先|要|还是|改票|改投|改成)?\s*(投|票|出|出掉|放逐)?\s*[1-8]\s*号?/.test(raw)
  )) return { action: "vote", target: seat };
  return { action: "other", target: seat };
}
function seatName(state, id) {
  const n = Number(id);
  for (const seat of state.seats || []) if (Number(seat.id) === n) return seat.name || (String(n) + "号");
  return id ? String(id) + "号" : "";
}
function seatRole(state, id) {
  const n = Number(id);
  for (const seat of state.seats || []) if (Number(seat.id) === n) return seat.role || "";
  return "";
}
function aliveWithout(state, id, reason, day) {
  const n = Number(id);
  state.alive = (state.alive || []).filter(function(x) { return Number(x) !== n; });
  for (const seat of state.seats || []) if (Number(seat.id) === n) {
    seat.alive = false;
    seat.death_reason = String(reason || seat.death_reason || "dead");
    seat.death_day = Number(day || seat.death_day || state.day || 0);
  }
}
function addPublic(state, line) {
  const log = Array.isArray(state.public_log) ? state.public_log.slice() : [];
  log.push(line);
  state.public_log = log;
}
function livingRoles(state, role) {
  return (state.seats || []).filter(function(s) { return s.alive === true && s.role === role; }).length;
}
function firstLivingRole(state, role) {
  for (const seat of state.seats || []) {
    if (seat.alive === true && seat.role === role) return Number(seat.id);
  }
  return 0;
}
function isAlive(state, id) {
  const n = Number(id);
  return (state.alive || []).map(Number).indexOf(n) >= 0;
}
function chooseNightKill(state) {
  const priority = [4, 6, 7, 2];
  for (const id of priority) {
    if (id !== Number(state.player_seat || 3) && isAlive(state, id) && seatRole(state, id) !== "werewolf") return id;
  }
  return 0;
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
      3: "玩家本人，只参与白天发言和投票。",
      4: "犹豫但会观察票型，倾向多听一轮。",
      5: "打圆场、转移焦点，削弱对自己的压力。",
      6: "谨慎分析公开发言和票型，不抢节奏。",
      7: "强势归票，抓住最清晰的公开矛盾推进投票。",
      8: "已出局时只在赛后旁观复盘。"
    };
    return map[Number(id)] || "根据公开信息发言。";
  }
  function privateSeatContextFor(id) {
    const seat = seatByID(id);
    const role = roleLabel(seat.role || "");
    const rows = (Array.isArray(state.seer_results) ? state.seer_results : []).filter(function(r) {
      return Number(r.seer) === Number(id);
    }).map(function(r) {
      const target = seatByID(r.target);
      return "第" + r.day + "夜查验结果：" + r.target + "号" + (target.name || "") + "为" + r.camp;
    });
    const seerText = rows.length ? rows.join(" | ") : "暂无";
    return "你的身份=" + role + "; 你的夜间查验=" + seerText + "; 只能使用自己的身份、自己的查验和公开信息发言，不得知道其他玩家的隐藏身份。";
  }
  function seatPublicCtx(id, text) {
    const history = recentHistory(6) || "暂无";
    const memory = String(board.getVar("public_memory") || "暂无");
    const focus = String(board.getVar("werewolf_public_focus") || "暂无");
    const seat = seatByID(id);
    const view = "座位=" + id + "; 姓名=" + (seat.name || "") + "; 存活=" + (seat.alive === true ? "true" : "false") + "; 发言风格=" + (seat.persona || "") + "; 本轮公开任务=" + seatPublicBehavior(id) + "; 私有视角=" + privateSeatContextFor(id);
    board.setVar("seat_" + id + "_public_view", view);
    return text + "\n\n当前公开焦点:\n" + focus + "\n\n公开角色视图:\n" + view + "\n\n公开记忆:\n" + memory + "\n\n近期历史:\n" + history;
  }
  ch("host_day_channel", "主持人白天公告。");
  ch("seat_1_channel", seatPublicCtx(1, "发言人=1号林知；用第一人称短话发言。"));
  ch("seat_2_channel", seatPublicCtx(2, "发言人=2号阿澈；用第一人称短话发言。"));
  ch("seat_4_channel", seatPublicCtx(4, "发言人=4号小满；用第一人称短话发言。"));
  ch("seat_5_channel", seatPublicCtx(5, "发言人=5号周岚；用第一人称短话发言。"));
  ch("seat_6_channel", seatPublicCtx(6, "发言人=6号陈医生；用第一人称短话发言。"));
  ch("seat_7_channel", seatPublicCtx(7, "发言人=7号老赵；用第一人称短话发言。"));
  ch("seat_8_channel", seatPublicCtx(8, "发言人=8号苏禾；用第一人称短话发言。"));
  ch("user_prompt_channel", "主持人提示3号玩家发言。");
  rawCh("vote_result_channel", String(state.vote_result_summary || ""));
  ch("vote_prompt_channel", "主持人投票提示，要求3号玩家明确投票座位号。");
}
function latestPublicTarget(state) {
  const focus = String(state.public_focus || "");
  const patterns = [
    /查验([1-8])号/,
    /围绕([1-8])号/,
    /怀疑([1-8])号/,
    /归票([1-8])号/,
    /投([1-8])号/
  ];
  for (const pattern of patterns) {
    const match = focus.match(pattern);
    if (match) return Number(match[1]);
  }
  return 0;
}
function firstAliveByRole(state, role, exclude) {
  const skip = {};
  for (const id of exclude || []) skip[String(id)] = true;
  for (const id of (state.alive || []).map(Number).sort(function(a, b) { return a - b; })) {
    if (!skip[String(id)] && seatRole(state, id) === role) return id;
  }
  return 0;
}
function firstAliveNotRole(state, role, exclude) {
  const skip = {};
  for (const id of exclude || []) skip[String(id)] = true;
  for (const id of (state.alive || []).map(Number).sort(function(a, b) { return a - b; })) {
    if (!skip[String(id)] && seatRole(state, id) !== role) return id;
  }
  return 0;
}
function chooseNPCVote(state, voter, userTarget) {
  const focusTarget = latestPublicTarget(state);
  const role = seatRole(state, voter);
  if (role === "werewolf") {
    if (focusTarget > 0 && focusTarget !== voter && seatRole(state, focusTarget) !== "werewolf" && isAlive(state, focusTarget)) return focusTarget;
    if (userTarget > 0 && userTarget !== voter && seatRole(state, userTarget) !== "werewolf" && isAlive(state, userTarget)) return userTarget;
    return firstAliveNotRole(state, "werewolf", [voter]);
  }
  if (focusTarget > 0 && focusTarget !== voter && isAlive(state, focusTarget)) return focusTarget;
  if (userTarget > 0 && userTarget !== voter && isAlive(state, userTarget)) return userTarget;
  return firstAliveByRole(state, "werewolf", [voter]) || firstAliveNotRole(state, role, [voter]);
}
function collectVotes(state, userTarget) {
  const votes = [];
  const player = Number(state.player_seat || 3);
  for (const voter of (state.alive || []).map(Number).sort(function(a, b) { return a - b; })) {
    const target = voter === player ? userTarget : chooseNPCVote(state, voter, userTarget);
    votes.push({ voter: voter, target: Number(target || 0) });
  }
  return votes;
}
function voteTargetName(state, id) {
  const n = Number(id || 0);
  return n > 0 ? n + "号" + seatName(state, n) : "弃票";
}
function voteLines(state, votes) {
  return votes.map(function(v) {
    return v.voter + "号" + seatName(state, v.voter) + "投" + voteTargetName(state, v.target);
  });
}
function chooseExileFromVotes(state, votes, userTarget) {
  const counts = {};
  for (const v of votes) {
    if (!v.target || !isAlive(state, v.target)) continue;
    counts[String(v.target)] = (counts[String(v.target)] || 0) + 1;
  }
  let best = 0;
  let bestCount = -1;
  for (const key of Object.keys(counts)) {
    const id = Number(key);
    const count = counts[key];
    if (count > bestCount || (count === bestCount && id === Number(userTarget))) {
      best = id;
      bestCount = count;
    }
  }
  return best || Number(userTarget || 0);
}
const state = board.getVar("werewolf_game_state") || {};
let move = parseVote(state.latest_user_text || "");
if (move.action !== "vote") {
  const classified = parseMove(msgText(board.getVar("werewolf_move_text")));
  if (classified.action === "vote" && classified.target) move = classified;
}
const target = Number(move.target || 0);
state.last_action = move.action;
state.last_target = move.target;
state.pending_tool_event = "";
state.pending_tool_detail = "";
if (move.action !== "vote" || target <= 0 || target === state.player_seat || (state.alive || []).map(Number).indexOf(target) < 0) {
  state.last_event = "need_vote_target";
  state.vote_retry_reason = "请明确投一个仍存活、且不是自己的座位号。";
  state.vote_valid = false;
  board.setVar("vote_valid", "false");
  syncStateViews(state);
} else {
  state.vote_valid = true;
  board.setVar("vote_valid", "true");
  const votedDay = state.day;
  const votes = collectVotes(state, target);
  const exileTarget = chooseExileFromVotes(state, votes, target);
  const voteText = voteLines(state, votes).join("；");
  state.vote_records = Array.isArray(state.vote_records) ? state.vote_records : [];
  state.vote_records.push({ day: votedDay, votes: votes, exiled: exileTarget });
  aliveWithout(state, exileTarget, "exile", votedDay);
  state.last_exile = exileTarget;
  const role = seatRole(state, exileTarget);
  addPublic(state, "第" + votedDay + "天投票：" + voteText + "。最高票为" + exileTarget + "号" + seatName(state, exileTarget) + "，被放逐。");
  state.last_event = "vote_exile";
  if (livingRoles(state, "werewolf") <= 0) {
    state.phase = "ended";
    state.winner = "good";
    state.pending_tool_event = "game_over";
    state.pending_tool_detail = "game_over_state_updated";
    state.vote_result_summary = "第" + votedDay + "天投票结束。\n票型：" + voteText + "。\n" + exileTarget + "号" + seatName(state, exileTarget) + "被放逐，身份为" + (role === "werewolf" ? "狼人" : "好人") + "。所有狼人出局，好人阵营获胜。";
    state.public_focus = "当前公开焦点：第" + votedDay + "天投票已放逐" + exileTarget + "号" + seatName(state, exileTarget) + "；所有狼人出局，好人阵营获胜。";
    addPublic(state, "好人阵营获胜。");
  } else {
    state.day += 1;
    state.phase = "night_open";
    state.pending_tool_event = "";
    state.pending_tool_detail = "";
    state.pending_night_kill = 0;
    state.pending_seer_target = 0;
    state.last_night_kill = 0;
    state.public_focus = "当前公开焦点：第" + votedDay + "天投票已结束，" + exileTarget + "号" + seatName(state, exileTarget) + "被放逐，身份暂不公开；接下来进入第" + state.day + "夜。";
    state.vote_result_summary = "第" + votedDay + "天投票结束。\n票型：" + voteText + "。\n" + exileTarget + "号" + seatName(state, exileTarget) + "被放逐，身份暂不公开。接下来进入第" + state.day + "夜。";
  }
  syncStateViews(state);
  board.setChannel("werewolf_tool_channel", [{
    role: "user",
    content: { parts: [{ type: "text", text: "Emit the configured Werewolf lifecycle event exactly as specified." }] }
  }]);
}
