const speaker = String(board.getVar("planned_speaker") || "narrator");
const latest = String(board.getVar("latest_user_text") || "");
const goal = String(board.getVar("story_step_goal") || "推进当前场景的一小步。");
const guidance = String(board.getVar("story_step_guidance") || "");
board.setVar("next_speaker", speaker);
board.setChannel("speech_channel", [{
  role: "user",
  content: { parts: [{
    type: "text",
    text: "speaker=" + speaker + "\nblueprint_step=" + String(board.getVar("story_step_name") || "") + "\nstory_brief=" + goal + "\nguidance=" + guidance + "\nlatest_user_text=" + latest,
  }] },
}]);
