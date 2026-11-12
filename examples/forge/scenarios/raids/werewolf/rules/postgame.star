# The end of the game: one announcement, then the table answers the human.
#
# The phase stays `ended`, so a restart has to come from a new session: the rules do
# not open a second game on their own.

_ROLE_LABELS = {"werewolf": "狼人", "seer": "预言家", "witch": "女巫",
                "hunter": "猎人", "villager": "平民"}


def _role_label(role):
    return _ROLE_LABELS.get(role, role)


def _facts(game):
    # The game is over, so nothing here is a secret: a recap that hides the roles is
    # a recap no one asked for. The seat still phrases the answer; the rules hand it
    # the truth to phrase.
    winner = "好人阵营" if game.get("winner") == "good" else "狼人阵营"
    lines = ["本局结果：" + winner + "获胜。", "座位与身份："]
    for seat in game["seats"]:
        mark = "存活" if seat["alive"] else "出局"
        lines.append("- " + str(seat["id"]) + "号" + seat["name"] + "：" +
                     _role_label(seat["role"]) + "，" + mark)
    log = list(game.get("public_log") or [])
    if log:
        lines.append("公开记录：")
        for line in log[-12:]:
            lines.append("- " + line)
    for record in game.get("vote_records") or []:
        rows = []
        for vote in record.get("votes") or []:
            target = "弃票" if vote["target"] == 0 else str(vote["target"]) + "号"
            rows.append(str(vote["voter"]) + "号→" + target)
        lines.append("第" + str(record.get("day")) + "天投票：" + "、".join(rows))
    return "\n".join(lines)


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
    brief = ("游戏已经结束，你现在在复盘桌上。这不是夜晚，不要给出刀人、查验或用药的决定；"
             + "请只依据下面的事实，用两三句话回答用户的复盘问题，不要编造没发生过的事。\n"
             + _facts(game) + "\n\n用户的复盘问题：" + _last_user(state["speech"]))
    return {"game": game, "brief": brief, "prompt": ""}
