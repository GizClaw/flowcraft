# The end of the game: one announcement, then the table answers the human.
#
# The phase stays `ended`, so a restart has to come from a new session: the rules do
# not open a second game on their own.

def _last_user(channel):
    for index in range(len(channel) - 1, -1, -1):
        if channel[index]["role"] == "user":
            return channel[index]["text"]
    return ""


def run(state):
    game = state["game"]
    if game.get("phase") != "ended":
        return {"game": game, "prompt": "", "brief": ""}
    label = "好人阵营" if game.get("winner") == "good" else "狼人阵营"
    if not game.get("post_game_announced"):
        game["post_game_announced"] = True
        game["post_game_turn"] = 0
        return {"game": game, "brief": "", "prompt": label + "获胜。可以继续提问复盘，或换一局。"}
    alive = game["alive"]
    if len(alive) == 0:
        return {"game": game, "brief": "", "prompt": ""}
    turn = game.get("post_game_turn") or 0
    seat = alive[turn % len(alive)]
    if seat == game["player_seat"] and len(alive) > 1:
        seat = alive[(turn + 1) % len(alive)]
    game["post_game_turn"] = turn + 1
    game["brief_seat"] = seat
    game["brief_kind"] = "postgame"
    return {"game": game, "brief": "游戏已经结束。用户的复盘问题：" + _last_user(state["speech"]), "prompt": ""}
