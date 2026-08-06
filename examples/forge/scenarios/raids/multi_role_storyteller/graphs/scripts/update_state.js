const speaker = String(board.getVar("next_speaker") || "narrator");
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
const speech = (msgText(board.getVar("tmp_speech_text")) || msgText(board.getVar("response"))).trim();
const story = board.getVar("story_state") || {};
const blueprintState = board.getVar("blueprint_state") || {};
const cast = board.getVar("cast_state") || {};
const user = board.getVar("user_state") || {};
const continuity = board.getVar("continuity_state") || {};
const ending = board.getVar("ending_state") || {};
const shortSpeech = speech.length > 80 ? speech.slice(0, 80) + "..." : speech;
const nextPhase = String(board.getVar("story_step_next_phase") || "");
const awaitUser = board.getVar("story_step_await_user") === true;

story.awaiting_user = awaitUser;
story.next_phase = awaitUser ? nextPhase : story.next_phase || "";
story.phase_spoken = Array.isArray(story.phase_spoken) ? story.phase_spoken : [];
story.phase_spoken = story.phase_spoken.concat([speaker]).slice(-8);
story.last_event = shortSpeech || story.last_event || "继续场景。";
if (String(story.phase || "") === "epilogue") {
  story.completed = true;
  story.epilogue_turns = Number(story.epilogue_turns || 0) + 1;
  ending.mode = "epilogue";
  ending.closed = true;
  ending.closure = "黄毛狼妖这一折已经结束，只能复盘本折因果或轻提示下一折。";
}
story.pressure = story.completed === true ? "主冲突已结束，旁白可以继续和用户对话或引出下一折。" :
  awaitUser ? "故事停在用户自然插话的位置。" :
  speaker === "monster" ? "门外来者还在狡辩，身份越来越可疑。" :
  speaker === "wukong" ? "悟空正在把怀疑变成行动。" :
  "破庙雨夜的疑点继续加深。";
blueprintState.current_phase = story.phase || blueprintState.current_phase || "";
blueprintState.next_phase = nextPhase || blueprintState.next_phase || "";
if (story.completed === true) {
  blueprintState.allowed_scope = "尾声只能讨论破庙雨夜黄毛狼妖这一折的已确认事实，或一句话带到下一折。";
  blueprintState.forbidden_scope = "不要继续给破庙这一折新增妖怪、同伙、法器、幕后主使、京城或新地点剧情。";
  continuity.open_threads = "本折主冲突已收束；用户若追问，只回答已确认因果。";
} else if (nextPhase === "epilogue") {
  ending.closure = "下一次用户接话后进入尾声，确认本折没有更多同伙或新危险。";
}
cast.last_speaker = speaker;
if (speaker === "monster") {
  cast.mood = "妖怪继续伪装，悟空更警觉，唐僧犹豫，八戒害怕。";
} else if (speaker === "wukong") {
  cast.mood = "悟空警觉上前，唐僧提醒谨慎，八戒想躲远一点。";
} else if (story.completed === true) {
  cast.mood = "主冲突已结束，旁白接住用户的尾声对话。";
} else if (awaitUser) {
  cast.mood = "众人等用户自然插话或判断下一步。";
}

const count = Number(board.getVar("internal_turn_count") || 0) + 1;
board.setVar("internal_turn_count", count);
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
board.setVar("tmp_story_state_line", "story_state: scene=" + (story.scene || "") + "; phase=" + (story.phase || "") + "; awaiting_user=" + (story.awaiting_user === true) + "; pressure=" + (story.pressure || "") + "; last_event=" + (story.last_event || ""));
board.setVar("tmp_blueprint_state_line", "blueprint_state: arc=" + (blueprintState.arc || "") + "; current_phase=" + (blueprintState.current_phase || "") + "; next_phase=" + (blueprintState.next_phase || "") + "; allowed_scope=" + (blueprintState.allowed_scope || "") + "; forbidden_scope=" + (blueprintState.forbidden_scope || ""));
board.setVar("tmp_cast_state_line", "cast_state: present=" + (cast.present || "") + "; last_speaker=" + (cast.last_speaker || "") + "; mood=" + (cast.mood || ""));
board.setVar("tmp_user_state_line", "user_state: last_input=" + (user.last_input || "") + "; last_choice=" + (user.last_choice || "") + "; preference=" + (user.preference || ""));
board.setVar("tmp_continuity_state_line", "continuity_state: confirmed=" + (continuity.confirmed || "") + "; open_threads=" + (continuity.open_threads || "") + "; forbidden_reveals=" + (continuity.forbidden_reveals || ""));
board.setVar("tmp_ending_state_line", "ending_state: mode=" + (ending.mode || "") + "; closed=" + (ending.closed === true) + "; closure=" + (ending.closure || "") + "; epilogue_policy=" + (ending.epilogue_policy || ""));
