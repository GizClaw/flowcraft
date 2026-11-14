# The exile: the table's target leaves, the hunter may take someone with them, and
# the game either ends or the next night begins.
#
# An exile checks the good side first: removing the last wolf ends the game even if
# the wolves would have reached parity the same moment.

def _seat(game, id):
    for seat in game["seats"]:
        if seat["id"] == id:
            return seat
    return {"id": id, "name": str(id) + "号", "role": "", "alive": False}


def _name(game, id):
    return _seat(game, id)["name"]


def _alive(game, id):
    return id in game["alive"]


def _log(game, line):
    log = list(game["public_log"])
    log.append(line)
    game["public_log"] = log


def _event(game, line):
    events = list(game.get("public_events") or [])
    events.append(line)
    game["public_events"] = events


def _kill(game, id, reason):
    for seat in game["seats"]:
        if seat["id"] != id:
            continue
        seat["alive"] = False
        seat["death_reason"] = reason
        seat["death_day"] = game["day"]
    game["alive"] = [alive for alive in game["alive"] if alive != id]


def _wolves_alive(game):
    count = 0
    for seat in game["seats"]:
        if seat["role"] == "werewolf" and seat["alive"]:
            count = count + 1
    return count


def _good_alive(game):
    count = 0
    for seat in game["seats"]:
        if seat["role"] != "werewolf" and seat["alive"]:
            count = count + 1
    return count


def _finish(game, order):
    wolves = _wolves_alive(game)
    good = _good_alive(game)
    good_win = wolves == 0
    wolf_win = good == 0 or wolves >= good
    winner = ""
    if order == "dawn":
        if wolf_win:
            winner = "werewolf"
        elif good_win:
            winner = "good"
    else:
        if good_win:
            winner = "good"
        elif wolf_win:
            winner = "werewolf"
    if winner != "":
        game["winner"] = winner
        game["phase"] = "ended"
        game["waiting_for"] = ""
        label = "好人阵营" if winner == "good" else "狼人阵营"
        _event(game, "游戏结束：" + label + "获胜。")
        _log(game, label + "获胜。")
        game["public_focus"] = "当前公开焦点：" + label + "获胜，游戏结束。"
        return
    game["day"] = game["day"] + 1
    game["phase"] = "night_wolf"
    game["waiting_for"] = ""
    game["night"] = {}
    game["speaker_order"] = []
    game["speech_index"] = 0
    game["speech_step"] = ""
    game["speech_seat"] = 0
    game["vote_pending"] = []
    game["vote_index"] = 0
    game["vote_step"] = ""
    game["vote_seat"] = 0
    game["current_votes"] = []
    game["pk"] = {"candidates": [], "mode": "", "round": 0}
    game["public_focus"] = "当前公开焦点：第" + str(game["day"]) + "夜正在进行。"


def run(state):
    game = state["game"]
    target = game.get("exile_target") or 0
    hunter = 0
    for seat in game["seats"]:
        if seat["role"] == "hunter":
            hunter = seat["id"]
    if target > 0 and _alive(game, target):
        _kill(game, target, "exile")
        game["last_exile"] = target
        _log(game, "第" + str(game["day"]) + "天：" + str(target) + "号" + _name(game, target) + "被放逐。")
        _event(game, "第" + str(game["day"]) + "天：" + str(target) + "号" + _name(game, target) + "被放逐。")
    if target == hunter and target > 0:
        game["hunter_pending"] = hunter
        game["phase"] = "hunter"
        game["waiting_for"] = ""
        return {"game": game, "prompt": ""}
    _finish(game, "day")
    return {"game": game, "prompt": ""}
