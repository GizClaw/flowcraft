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
const raw = msgText(board.getVar("speaker_select_text"));
const ctx = board.getVar("story_phase_context") || {};
const allowed = Array.isArray(ctx.allowed_speakers) ? ctx.allowed_speakers : ["narrator"];
function field(name, fallback) {
  const re = new RegExp("(^|[;；\\s])" + name + "\\s*=\\s*([^;；]+)", "i");
  const m = re.exec(raw);
  return m ? String(m[2] || "").trim() : fallback;
}
const requestedSpeaker = field("speaker", ctx.first_speaker_hint || "narrator").toLowerCase();
let speaker = requestedSpeaker;
let guidance = field("guidance", "按当前蓝图目标自然推进这一小步，不要另起新线。");
if (allowed.indexOf(speaker) < 0) {
  speaker = allowed.indexOf("narrator") >= 0 ? "narrator" : allowed[0] || "narrator";
  guidance = "按最终发言身份改写这一拍，只推进当前蓝图目标，不沿用被纠正 speaker 的口吻。";
}
let awaitUser = /await_user\s*=\s*true/i.test(raw);
const goal = String(ctx.goal || "推进当前场景的一小步。");
const guard = String(ctx.guard || "不要偏离当前蓝图阶段。");
const blueprintState = board.getVar("blueprint_state") || {};
const continuity = board.getVar("continuity_state") || {};
const ending = board.getVar("ending_state") || {};
if (String(ctx.phase || "") === "epilogue") {
  speaker = "narrator";
  awaitUser = true;
  guidance = "只回应本折已确认事实，说明黄毛狼妖事件已经收住，不再给破庙这一折新增妖怪或反转";
}
const spoken = Array.isArray(ctx.phase_spoken) ? ctx.phase_spoken : [];
const handoffAfter = Number(ctx.handoff_after || 0);
if (handoffAfter > 0 && spoken.length + 1 >= handoffAfter && allowed.indexOf("narrator") >= 0) {
  speaker = "narrator";
  awaitUser = true;
  if (requestedSpeaker !== "narrator") {
    guidance = "旁白承接刚才的角色动作，推进镜头并自然停在用户可以插话的位置。";
  }
}
guidance = guidance.replace(/[。.!！?？]+$/, "");
function cleanText(value) {
  return String(value || "").replace(/[。.!！?？]+$/, "");
}
const latest = String(board.getVar("latest_user_text") || "");
const cleanGoal = cleanText(goal);
const cleanGuard = cleanText(guard);
const boundary = "阶段目标=" + cleanGoal +
  "。阶段边界=" + cleanGuard +
  "。剧情引导=" + guidance +
  "。连续性事实=" + cleanText(continuity.confirmed) +
  "。禁止突破=" + cleanText(continuity.forbidden_reveals) +
  "。结尾策略=" + cleanText(ending.epilogue_policy) +
  "。当前发言身份=" + speaker +
  "。只推进当前蓝图一小步，不要固定轮流报幕，不要突破 memory layout 的边界。";
board.setVar("planned_speaker", speaker);
board.setVar("story_step_name", String(ctx.phase || "scene") + ":" + speaker);
board.setVar("story_step_goal", boundary);
board.setVar("story_step_guidance", guidance);
board.setVar("story_step_await_user", awaitUser);
