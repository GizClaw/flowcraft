const state = board.getVar("werewolf_game_state") || {};
let next = "";
const wf = String(state.waiting_for || "");
if (wf === "wolf_target") next = "night_wolf";
else if (wf === "witch_action") next = "night_witch";
else if (wf === "seer_action") next = "night_seer";
else if (wf === "hunter_shot") next = "hunter";
else if (wf === "day_speech") next = "day";
else if (wf === "vote" || wf === "pk_vote") next = "vote";
else if (wf === "pk_speech") next = "pk";
else {
  switch (state.phase) {
    case "night_wolf": next = "night_wolf"; break;
    case "night_witch": next = "night_witch"; break;
    case "night_seer": next = "night_seer"; break;
    case "dawn": next = "dawn"; break;
    case "hunter": next = "hunter"; break;
    case "day": next = "day"; break;
    case "vote": next = "vote"; break;
    case "tally": next = "tally"; break;
    case "pk": next = "pk"; break;
    case "exile": next = "exile"; break;
    case "ended": next = "ended"; break;
    default: next = "night_wolf";
  }
}
state.next_rule = next;
board.setVar("werewolf_next_rule", next);
board.setVar("werewolf_game_state", state);
