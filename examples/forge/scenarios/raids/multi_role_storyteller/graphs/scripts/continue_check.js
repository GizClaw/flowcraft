const count = Number(board.getVar("internal_turn_count") || 0);
const story = board.getVar("story_state") || {};
const shouldContinue = story.awaiting_user !== true && count < 4;
board.setVar("continue_story", shouldContinue);
