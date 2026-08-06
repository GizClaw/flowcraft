const story = board.getVar("story_state") || {};
const blueprintState = board.getVar("blueprint_state") || {};
const continuity = board.getVar("continuity_state") || {};
const ending = board.getVar("ending_state") || {};
const user = board.getVar("user_state") || {};
const latest = String(board.getVar("latest_user_text") || "").trim();
const blueprint = {
  threshold: {
    name: "threshold",
    goal: "破庙雨夜，门外异常敲门。重点是建立气氛、角色分歧和是否应门的张力。",
    guard: "只能建立破庙雨夜和敲门异常，不揭示最终妖身。",
    allowed: ["narrator", "wukong", "tangseng", "bajie"],
    first: "narrator",
    handoff_after: 3,
    next_phase: "reveal",
  },
  reveal: {
    name: "reveal",
    goal: "根据用户介入推进开门、隔门试探或观察，只露出黄毛货郎的可疑状态和篮中异样。",
    guard: "可以出现黄毛、黑腥水、篮子、脚印；不要说他已经是某个大妖，也不要新增幕后势力。",
    allowed: ["wukong", "narrator", "monster", "bajie", "tangseng"],
    first: "narrator",
    handoff_after: 4,
    next_phase: "probe",
  },
  probe: {
    name: "probe",
    goal: "悟空试探货郎，妖怪继续装可怜，唐僧担心误伤，八戒怕惹祸，疑点继续累积。",
    guard: "这一阶段只做试探，不动手定输赢；妖怪仍不承认身份。",
    allowed: ["wukong", "monster", "tangseng", "bajie", "narrator"],
    first: "wukong",
    handoff_after: 4,
    next_phase: "pressure",
  },
  pressure: {
    name: "pressure",
    goal: "妖怪反咬或诱骗进庙，悟空发现更多破绽，唐僧与八戒形成分歧，让冲突升级但不变成解谜问答。",
    guard: "可以让妖怪狡辩或诱骗；不要引入第二只妖怪；不要把场景带出破庙。",
    allowed: ["monster", "wukong", "tangseng", "bajie", "narrator"],
    first: "monster",
    handoff_after: 4,
    next_phase: "exposure",
  },
  exposure: {
    name: "exposure",
    goal: "关键破绽出现：黄毛、黑腥水、狼爪影子或脚印暴露，悟空确认货郎不是凡人。",
    guard: "只确认黄毛狼妖伪装，不扩展到黑熊、貂鼠、神将、京城或其他新主线。",
    allowed: ["narrator", "wukong", "monster", "tangseng", "bajie"],
    first: "narrator",
    handoff_after: 4,
    next_phase: "clash",
  },
  clash: {
    name: "clash",
    goal: "冲突进入动作段：悟空逼近，妖怪露怯，货郎伪装开始撑不住，用户可以自然插话改变打法。",
    guard: "动作只围绕破庙门口和狼妖伪装；不要一棒直接跳到新故事。",
    allowed: ["wukong", "monster", "narrator", "bajie", "tangseng"],
    first: "wukong",
    handoff_after: 4,
    next_phase: "turning",
  },
  turning: {
    name: "turning",
    goal: "妖怪露出狼形但还想逃或求饶，唐僧要求不要滥杀，八戒害怕又嘴硬。",
    guard: "可以出现狼耳、狼爪、黄毛狼妖原形；不要新增同伙或幕后山主。",
    allowed: ["monster", "tangseng", "wukong", "bajie", "narrator"],
    first: "monster",
    handoff_after: 4,
    next_phase: "resolution",
  },
  resolution: {
    name: "resolution",
    goal: "收束主冲突：黄毛狼妖被悟空制住后逐走或罚走，交代为何夜雨敲门，唐僧与八戒各自给出反应。",
    guard: "这是本折收束。只解决黄毛狼妖，不打死狼妖，不再抛新敌人、新法器、新地点。",
    allowed: ["narrator", "wukong", "monster", "tangseng", "bajie"],
    first: "wukong",
    handoff_after: 4,
    next_phase: "aftermath",
  },
  aftermath: {
    name: "aftermath",
    goal: "余波：破庙雨声渐小，确认狼妖已被逐走且没有更多危险，天明继续上路，给这一折明确结束感。",
    guard: "必须确认本折没有更多妖怪或同伙；不要改变狼妖未被打死的结局；只能轻轻提示下一折在远处，不展开。",
    allowed: ["narrator", "tangseng", "bajie", "wukong"],
    first: "narrator",
    handoff_after: 3,
    next_phase: "epilogue",
  },
  epilogue: {
    name: "epilogue",
    goal: "主冲突已经结束。旁白进入尾声对话，只回应用户对本折的追问、复盘因果，或用一句话轻轻引下一折。",
    guard: "尾声禁止新增本折敌人、反转、同伙、法器或新地点剧情。用户问有没有更多妖怪时，说明这一折已经收住。",
    allowed: ["narrator"],
    first: "narrator",
    handoff_after: 1,
    next_phase: "epilogue",
  },
};

if (story.awaiting_user && latest) {
  user.last_choice = latest;
  story.awaiting_user = false;
  story.phase = story.next_phase || story.phase || "threshold";
  story.phase_spoken = [];
  story.next_phase = "";
  if (story.phase === "epilogue") {
    story.completed = true;
    story.pressure = "主冲突已结束，进入旁白尾声对话。";
    ending.mode = "epilogue";
    ending.closed = true;
  }
}
const phaseName = blueprint[story.phase] ? story.phase : "threshold";
const phase = blueprint[phaseName];
blueprintState.current_phase = phase.name;
blueprintState.phase_goal = phase.goal;
blueprintState.phase_guard = phase.guard;
blueprintState.next_phase = phase.next_phase;
ending.mode = phase.name === "epilogue" ? "epilogue" : "active_story";
const spoken = Array.isArray(story.phase_spoken) ? story.phase_spoken : [];
let turnAllowed = phase.allowed.slice();
const handoffAfter = Number(phase.handoff_after || 0);
const handoffDue = handoffAfter > 0 && spoken.length + 1 >= handoffAfter;
if (phase.name === "epilogue") {
  turnAllowed = ["narrator"];
} else if (spoken.length === 0 && phase.first) {
  turnAllowed = [phase.first];
} else if (handoffDue && phase.allowed.indexOf("narrator") >= 0) {
  turnAllowed = ["narrator"];
} else {
  const unused = phase.allowed.filter(function(speaker) {
    return spoken.indexOf(speaker) < 0;
  });
  if (unused.length > 0) turnAllowed = unused;
}

const phaseContext = {
  phase: phase.name,
  goal: phase.goal,
  guard: phase.guard,
  allowed_speakers: turnAllowed,
  blueprint_allowed_speakers: phase.allowed,
  first_speaker_hint: phase.first,
  handoff_after: phase.handoff_after,
  next_phase: phase.next_phase,
  phase_spoken: spoken,
  internal_turn_count: Number(board.getVar("internal_turn_count") || 0),
  last_speaker: String((board.getVar("cast_state") || {}).last_speaker || ""),
  latest_user_text: latest,
  blueprint_contract: String(blueprintState.contract || ""),
  continuity_confirmed: String(continuity.confirmed || ""),
  continuity_forbidden: String(continuity.forbidden_reveals || ""),
  ending_mode: String(ending.mode || "active_story"),
  ending_policy: String(ending.epilogue_policy || ""),
  story_completed: story.completed === true,
};
board.setVar("story_phase_context", phaseContext);
board.setVar("story_phase_context_text", JSON.stringify(phaseContext, null, 2));
board.setVar("story_step_next_phase", phase.next_phase);
board.setVar("blueprint_state", blueprintState);
board.setVar("continuity_state", continuity);
board.setVar("ending_state", ending);
board.setVar("story_state", story);
board.setVar("user_state", user);
board.setVar("blueprint_state_text", JSON.stringify(blueprintState, null, 2));
board.setVar("continuity_state_text", JSON.stringify(continuity, null, 2));
board.setVar("ending_state_text", JSON.stringify(ending, null, 2));
board.setChannel("speaker_select_channel", [{
  role: "user",
  content: { parts: [{ type: "text", text: JSON.stringify(phaseContext, null, 2) }] },
}]);
