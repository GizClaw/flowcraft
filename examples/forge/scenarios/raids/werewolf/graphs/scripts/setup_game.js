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
    seats: (state.seats || []).map(function(s) {
      return { id: s.id, name: s.name, role: s.role, persona: s.persona };
    })
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
  function seatPrivateCtx(id, text) {
    const memory = String(board.getVar("seat_" + id + "_memory") || "暂无");
    const publicMemory = String(board.getVar("public_memory") || "暂无");
    const seat = seatByID(id);
    const status = "seat=" + id + "; name=" + (seat.name || "") + "; role=" + roleLabel(seat.role || "") + "; alive=" + (seat.alive === true ? "true" : "false") + "; death_reason=" + (seat.death_reason || "none") + "; death_day=" + (seat.death_day || "none") + "; persona=" + (seat.persona || "");
    const contract = "post_game_contract: phase=ended 时只复盘本局；role_assignment 不变；不创建新局；不新增夜间行动、查验、投票或技能结算；seat_state 与 seat_memory 中的 alive/death_reason/death_day 是每个角色出局原因的事实来源；只引用当前 state、public_log、seer_results、seat_memory 和 history 中已有的事实；output_format=plain_text。";
    const factCheck = "seat_fact_check: death_reason=" + (seat.death_reason || "none") + "; death_day=" + (seat.death_day || "none") + "; alive=" + (seat.alive === true ? "true" : "false") + "; 出局后的内容属于赛后旁观复盘，不是游戏内发言、投票、查验或夜间行动。";
    function ownHistoryForSeat(seatID) {
      const nodeNames = {
        1: ["seat_1_lin_zhi_speak"],
        2: ["seat_2_ache_speak"],
        4: ["seat_4_xiaoman_speak"],
        5: ["seat_5_zhoulan_speak"],
        6: ["seat_6_chen_doctor_speak"],
        7: ["seat_7_laozhao_speak"],
        8: ["seat_8_suhe_speak"]
      }[Number(seatID)] || [];
      const seen = {};
      for (const name of nodeNames) seen[name] = true;
      const msgs = board.channel(board.MAIN_CHANNEL) || [];
      return msgs.map(function(msg) {
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
        if (!seen[node] || !text) return "";
        return node + ": " + text.slice(0, 260);
      }).filter(Boolean).slice(-4).join("\n") || "none";
    }
    board.setVar("post_game_contract", contract);
    return text + "\n\n赛后角色状态:\n" + status + "\n\n赛后事实核对:\n" + factCheck + "\n\n赛后状态约束:\n" + contract + "\n\n角色私有记忆:\n" + memory + "\n\n公开记忆:\n" + publicMemory + "\n\n本角色历史发言:\n" + ownHistoryForSeat(id);
  }
  ch("host_opening_channel", "主持人开局公告。");
  ch("host_day_channel", "主持人白天公告。");
  ch("seat_1_channel", seatPublicCtx(1, "发言人=1号林知；用第一人称短话发言。"));
  ch("seat_2_channel", seatPublicCtx(2, "发言人=2号阿澈；用第一人称短话发言。"));
  ch("seat_4_channel", seatPublicCtx(4, "发言人=4号小满；用第一人称短话发言。"));
  ch("seat_5_channel", seatPublicCtx(5, "发言人=5号周岚；用第一人称短话发言。"));
  ch("seat_6_channel", seatPublicCtx(6, "发言人=6号陈医生；用第一人称短话发言。"));
  ch("seat_7_channel", seatPublicCtx(7, "发言人=7号老赵；用第一人称短话发言。"));
  ch("seat_8_channel", seatPublicCtx(8, "发言人=8号苏禾；用第一人称短话发言。"));
  ch("user_prompt_channel", "主持人提示3号玩家发言。");
  ch("vote_prompt_channel", "主持人投票提示，要求3号玩家明确投票座位号。");
  rawCh("vote_result_channel", String(state.vote_result_summary || ""));
  ch("retry_channel", "主持人纠正当前动作。");
  ch("post_game_seat_1_channel", seatPrivateCtx(1, "赛后自由发言，角色=1号林知。"));
  ch("post_game_seat_2_channel", seatPrivateCtx(2, "赛后自由发言，角色=2号阿澈。"));
  ch("post_game_seat_4_channel", seatPrivateCtx(4, "赛后自由发言，角色=4号小满。"));
  ch("post_game_seat_5_channel", seatPrivateCtx(5, "赛后自由发言，角色=5号周岚。"));
  ch("post_game_seat_6_channel", seatPrivateCtx(6, "赛后自由发言，角色=6号陈医生。"));
  ch("post_game_seat_7_channel", seatPrivateCtx(7, "赛后自由发言，角色=7号老赵。"));
  ch("post_game_seat_8_channel", seatPrivateCtx(8, "赛后自由发言，角色=8号苏禾。"));
  ch("post_game_host_channel", "赛后自由发言收束，角色=主持人。");
}
function seededRandom(seed) {
  let h = 2166136261;
  const s = String(seed || "");
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return function() {
    h += 0x6D2B79F5;
    let t = h;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}
function shuffle(list, rand) {
  const out = list.slice();
  for (let i = out.length - 1; i > 0; i--) {
    const j = Math.floor(rand() * (i + 1));
    const tmp = out[i];
    out[i] = out[j];
    out[j] = tmp;
  }
  return out;
}
const roleSeed = "werewolf-" + Date.now();
const rand = seededRandom(roleSeed);
const assignedUserRole = "villager";
let removedUserCard = false;
const npcRoles = ["werewolf", "werewolf", "werewolf", "villager", "villager", "seer", "witch", "hunter"].filter(function(role) {
  if (!removedUserCard && role === assignedUserRole) {
    removedUserCard = true;
    return false;
  }
  return true;
});
const assignedRoles = shuffle(npcRoles, rand);
const baseSeats = [
  { id: 1, name: "林知", persona: "谨慎、会反咬、擅长装好人" },
  { id: 2, name: "阿澈", persona: "逻辑清楚但语气不强势，愿意公开报验" },
  { id: 3, name: "你", persona: "用户，可白天发言、投票，也可以诈身份" },
  { id: 4, name: "小满", persona: "容易犹豫，重视票型" },
  { id: 5, name: "周岚", persona: "爱打圆场、转移焦点、缓和矛盾" },
  { id: 6, name: "陈医生", persona: "保守，倾向多听一轮" },
  { id: 7, name: "老赵", persona: "直接、强硬、喜欢拍桌归票" },
  { id: 8, name: "苏禾", persona: "安静，容易被忽视，但复盘时会补充细节" }
];
let roleIndex = 0;
const seats = baseSeats.map(function(seat) {
  const role = seat.id === 3 ? assignedUserRole : assignedRoles[roleIndex++];
  return Object.assign({}, seat, { role: role, alive: true });
});
function firstSeatByRole(role) {
  for (const seat of seats) if (seat.role === role) return Number(seat.id);
  return 0;
}
const state = {
  started: true,
  phase: "night_open",
  day: 1,
  player_seat: 3,
  player_role: seats[2].role,
  role_seed: roleSeed,
  seer_seat: firstSeatByRole("seer"),
  witch_seat: firstSeatByRole("witch"),
  hunter_seat: firstSeatByRole("hunter"),
  seats: seats,
  alive: [1, 2, 3, 4, 5, 6, 7, 8],
  public_log: [
    "第1夜：游戏开始，主持人进入夜间流程。"
  ],
  private_notes: ["你是3号玩家，身份由 role_assignment 记录；夜间行动由主持流程自动推进。"],
  seer_results: [],
  last_event: "setup_day_open",
  last_action: "start",
  last_target: "",
  last_night_kill: 0,
  pending_night_kill: 0,
  pending_seer_target: 0,
  witch_used_poison: false,
  witch_used_save: false,
  pending_tool_event: "setup",
  pending_tool_detail: "setup_state_initialized",
  winner: "",
  public_focus: "当前公开焦点：第1夜正在进行，白天信息尚未公布。"
};
syncStateViews(state);
board.setChannel("werewolf_tool_channel", [{
  role: "user",
  content: { parts: [{ type: "text", text: "Emit the configured Werewolf lifecycle event exactly as specified." }] }
}]);
