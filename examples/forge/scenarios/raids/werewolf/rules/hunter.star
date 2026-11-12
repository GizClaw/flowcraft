# The hunter's shot: the last thing one death may do.
#
# It runs in two entries because it is a question followed by an answer: the first
# asks the seat (through the perform rule), the second applies what came back.

def _name(game, id):
    for seat in game["seats"]:
        if seat["id"] == id:
            return seat["name"]
    return str(id) + "号"


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


def _wolves(game):
    count = 0
    for seat in game["seats"]:
        if seat["role"] == "werewolf" and seat["alive"]:
            count = count + 1
    return count


def _good(game):
    count = 0
    for seat in game["seats"]:
        if seat["role"] != "werewolf" and seat["alive"]:
            count = count + 1
    return count


def _finish(game):
    wolves = _wolves(game)
    good = _good(game)
    good_win = wolves == 0
    wolf_win = good == 0 or wolves >= good
    if good_win:
        game["winner"] = "good"
    elif wolf_win:
        game["winner"] = "werewolf"
    if game["winner"] != "":
        game["phase"] = "ended"
        game["waiting_for"] = ""
        label = "好人阵营" if game["winner"] == "good" else "狼人阵营"
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
    game["vote_pending"] = []
    game["vote_index"] = 0
    game["current_votes"] = []
    game["pk"] = {"candidates": [], "mode": "", "round": 0}
    game["public_focus"] = "当前公开焦点：第" + str(game["day"]) + "夜正在进行。"


def run(state):
    game = state["game"]
    if game.get("phase") != "hunter":
        return {"game": game, "brief": "", "prompt": ""}
    pending = game.get("hunter_pending") or 0
    if pending == 0:
        _finish(game)
        return {"game": game, "brief": "", "prompt": ""}
    if game.get("hunter_target") != None:
        target = game.get("hunter_target") or 0
        if target > 0 and target in game["alive"] and target != pending:
            _kill(game, target, "hunter")
            _log(game, str(target) + "号" + _name(game, target) + "被猎人带走。")
            _event(game, str(target) + "号" + _name(game, target) + "被猎人带走。")
        game["hunter_pending"] = 0
        game["hunter_target"] = None
        _finish(game)
        return {"game": game, "brief": "", "prompt": ""}
    game["brief_seat"] = pending
    game["brief_kind"] = "hunter"
    return {"game": game, "brief": "你是猎人，你出局了。请选择开枪目标：target=X（不带走任何人写 target=0）。", "prompt": ""}
