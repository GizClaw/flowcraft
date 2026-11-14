# Dawn resolves the night: who died, whether the hunter must shoot, and whether
# the game is over.
#
# The order of the win checks is the old scenario's: at dawn the wolves' win is
# checked first (a night that leaves the wolves at parity ends the game), while an
# exile checks the good side first (the last wolf leaving ends it).

def _seat(game, id):
    for seat in game["seats"]:
        if seat["id"] == id:
            return seat
    return {"id": id, "name": str(id) + "号", "role": "", "alive": False}


def _name(game, id):
    return _seat(game, id)["name"]


def _alive(game, id):
    return id in game["alive"]


def _alive_ids(game):
    return game["alive"]


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
    game["phase"] = "day"
    game["waiting_for"] = ""
    game["speaker_order"] = _alive_ids(game)
    game["speech_index"] = 0
    names = []
    for id in _alive_ids(game):
        names.append(str(id) + "号" + _name(game, id))
    game["public_focus"] = "当前公开焦点：第" + str(game["day"]) + "天白天，仍存活玩家按座位顺序发言：" + "、".join(names) + "。"


def run(state):
    game = state["game"]
    night = game.get("night") or {}
    knife = night.get("wolf_target") or 0
    save = night.get("witch_save") or 0
    poison = night.get("witch_poison") or 0
    saved = knife > 0 and save == knife

    deaths = []
    if knife > 0 and not saved and _alive(game, knife):
        deaths.append({"seat": knife, "reason": "night_kill"})
    if poison > 0 and _alive(game, poison) and poison != knife:
        deaths.append({"seat": poison, "reason": "witch_poison"})
    for death in deaths:
        _kill(game, death["seat"], death["reason"])
    game["last_night_kill"] = knife if (knife > 0 and not saved) else 0

    if len(deaths) > 0:
        names = []
        reasons = []
        for death in deaths:
            names.append(str(death["seat"]) + "号" + _name(game, death["seat"]))
            reasons.append(str(death["seat"]) + "号" + _name(game, death["seat"]) + ("被毒死" if death["reason"] == "witch_poison" else "被狼刀死"))
        _log(game, "第" + str(game["day"]) + "夜：夜晚结束，" + "、".join(names) + "死亡。")
        _event(game, "第" + str(game["day"]) + "夜：" + "、".join(reasons) + "。")
    else:
        _log(game, "第" + str(game["day"]) + "夜：夜晚结束，无人死亡。")
        _event(game, "第" + str(game["day"]) + "夜：平安夜，无人死亡。")

    hunter = 0
    for seat in game["seats"]:
        if seat["role"] == "hunter":
            hunter = seat["id"]
    hunter_died = False
    for death in deaths:
        if death["seat"] == hunter and death["reason"] != "witch_poison":
            hunter_died = True
    game["night"] = {}
    if hunter_died:
        game["hunter_pending"] = hunter
        game["phase"] = "hunter"
        game["waiting_for"] = ""
        return {"game": game, "prompt": ""}
    _finish(game, "dawn")
    return {"game": game, "prompt": ""}
