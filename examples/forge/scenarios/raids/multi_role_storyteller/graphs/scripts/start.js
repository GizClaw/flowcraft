function latestUserText() {
  const msgs = board.channel(board.MAIN_CHANNEL) || [];
  for (let i = msgs.length - 1; i >= 0; i--) {
    const msg = msgs[i] || {};
    if (msg.role !== "user") continue;
    if (typeof msg.content === "string" && msg.content.trim()) return msg.content.trim();
    const parts = Array.isArray(msg.content.parts) ? msg.content.parts : [];
    const text = parts.map(function(part) {
      return part && part.type === "text" && typeof part.text === "string" ? part.text : "";
    }).join("").trim();
    if (text) return text;
  }
  return "";
}

const story = board.getVar("story_state") || {
  scene: "取经路上的破庙雨夜",
  phase: "threshold",
  phase_spoken: [],
  next_phase: "",
  awaiting_user: false,
  completed: false,
  epilogue_turns: 0,
  pressure: "门外有人在雨里敲门，但声音不像普通人。",
  last_event: "故事尚未开场。",
};
const blueprintState = board.getVar("blueprint_state") || {
  arc: "破庙雨夜黄毛货郎",
  contract: "固定一折短故事：门外黄毛货郎其实是黄毛狼妖伪装；围绕敲门、试探、露馅、制伏、逐走、天明上路推进。",
  current_phase: story.phase || "threshold",
  allowed_scope: "只允许破庙雨夜、黄毛货郎/狼妖、取经四人、篮中黑腥水/黄毛/脚印等本折线索。",
  forbidden_scope: "不要新增狮驼岭、黑熊精、貂鼠、天宫神将、铜铃残魂、京城、另一个大妖或新地点主线。",
};
const cast = board.getVar("cast_state") || {
  present: "narrator,wukong,tangseng,bajie,monster",
  last_speaker: "",
  mood: "悟空警觉，唐僧谨慎，八戒怕麻烦，门外妖怪伪装虚弱。",
};
const user = board.getVar("user_state") || {
  last_input: "",
  last_choice: "",
  preference: "喜欢节奏快、具体、有角色冲突的西游对话。",
};
const continuity = board.getVar("continuity_state") || {
  confirmed: "地点=破庙雨夜；门外人=黄毛货郎伪装；真实身份=黄毛狼妖；结局约束=狼妖被制伏后逐走或罚走，不打死；固定角色=旁白/悟空/唐僧/八戒/妖怪。",
  open_threads: "敲门异常；黄毛与黑腥水；货郎为何夜雨来庙；是否开门；悟空如何试探。",
  forbidden_reveals: "不要把妖怪改成其他种类；不要说还有幕后大妖；不要把故事转去别的地点。",
};
const ending = board.getVar("ending_state") || {
  mode: "active_story",
  closed: false,
  closure: "",
  epilogue_policy: "主冲突结束后只复盘本折因果或轻轻引下一折，不再给本折新增敌人。",
};
const latest = latestUserText();
if (latest) user.last_input = latest;
board.setVar("story_state", story);
board.setVar("blueprint_state", blueprintState);
board.setVar("cast_state", cast);
board.setVar("user_state", user);
board.setVar("continuity_state", continuity);
board.setVar("ending_state", ending);
board.setVar("story_state_text", JSON.stringify(story, null, 2));
board.setVar("blueprint_state_text", JSON.stringify(blueprintState, null, 2));
board.setVar("cast_state_text", JSON.stringify(cast, null, 2));
board.setVar("user_state_text", JSON.stringify(user, null, 2));
board.setVar("continuity_state_text", JSON.stringify(continuity, null, 2));
board.setVar("ending_state_text", JSON.stringify(ending, null, 2));
board.setVar("latest_user_text", latest);
board.setVar("internal_turn_count", 0);
board.setVar("continue_story", true);
