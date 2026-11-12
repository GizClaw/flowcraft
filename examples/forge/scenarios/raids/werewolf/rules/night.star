# Night rules: the wolves choose a victim, the witch may save or poison, the seer
# checks a seat.
#
# The rule is pure state plus the channel the pending answer came from. It parses
# what was said, applies it, and decides what happens next; the private channels
# are separate from public speech, so a decision never becomes public by accident.

def _seat(game, id):
    for seat in game["seats"]:
        if seat["id"] == id:
            return seat
    return {"id": id, "name": str(id) + "号", "role": "", "alive": False}


def _name(game, id):
    return _seat(game, id)["name"]


def _role(game, id):
    return _seat(game, id)["role"]


def _alive(game, id):
    return id in game["alive"]


def _alive_ids(game):
    return game["alive"]


def _role_label(role):
    labels = {"werewolf": "狼人", "seer": "预言家", "witch": "女巫", "hunter": "猎人", "villager": "平民"}
    return labels.get(role, role)


def _role_seat(game, role, alive_only):
    for seat in game["seats"]:
        if seat["role"] != role:
            continue
        if alive_only and not seat["alive"]:
            continue
        return seat["id"]
    return 0


def _log(game, line):
    log = list(game["public_log"])
    log.append(line)
    game["public_log"] = log


def _numbers(text):
    out = []
    cur = ""
    for ch in text.elems():
        if ch >= "0" and ch <= "9":
            cur = cur + ch
        elif cur != "":
            out.append(int(cur))
            cur = ""
    if cur != "":
        out.append(int(cur))
    return out


_NUMERALS = {"一": 1, "二": 2, "两": 2, "三": 3, "四": 4, "五": 5,
             "六": 6, "七": 7, "八": 8, "九": 9, "十": 10}


def _seat_number(text, start):
    # Seats answer in Chinese: "我投六号" is as common as "我投6号". A parser that
    # only reads digits turns every one of those into a 0 - a vote for six becomes an
    # abstention, and a wolf's "刀六号" becomes "whoever is first in the alive list".
    numbers = _numbers(text[start:])
    if numbers:
        return numbers[0]
    rest = text[start:]
    for index in range(len(rest)):
        if rest[index] in _NUMERALS:
            return _NUMERALS[rest[index]]
    return 0


def _target(text):
    at = text.find("target=")
    if at >= 0:
        number = _seat_number(text, at)
        if number:
            return number
    number = _seat_number(text, 0)
    if number >= 1 and number <= 8:
        return number
    return 0


def _witch_action(text):
    if "不用药" in text or "不救不毒" in text or "放弃" in text or "action=none" in text:
        return "none", 0
    target = _target(text)
    save = ("救" in text or "解药" in text) and "不救" not in text
    poison = "毒" in text and "不毒" not in text
    if save and poison:
        return "invalid", target
    if save:
        return "save", target
    if poison:
        return "poison", target
    if target == 0:
        return "none", 0
    return "invalid", target


def _last_said(channel, role):
    for index in range(len(channel) - 1, -1, -1):
        if channel[index]["role"] == role:
            return channel[index]["text"]
    return ""


def _night(game):
    if not game.get("night") or not game["night"].get("started"):
        game["night"] = {
            "started": True,
            "wolf_slot": 0,
            "wolf_proposals": [],
            "wolf_discussion": [],
            "wolf_decided": False,
            "wolf_target": 0,
            "witch_save": 0,
            "witch_poison": 0,
            "witch_decided": False,
            "seer_target": 0,
            "seer_decided": False,
        }
        _log(game, "第" + str(game["day"]) + "夜：狼人请睁眼。")
    return game["night"]


def _context(game):
    # A seat is asked about the *table*, not about a rule name: the day, who is
    # alive and what was just said. Without this the brief is the same string every
    # night, the delegate call's arguments hash matches the last one, and the script
    # replay answers with the first night's decision instead of asking again.
    alive = []
    for seat in game["seats"]:
        if seat["alive"]:
            alive.append(str(seat["id"]) + "号" + seat["name"])
    lines = ["第" + str(game["day"]) + "天。存活：" + "、".join(alive) + "。"]
    recent = list(game.get("public_log") or [])[-6:]
    if recent:
        lines.append("最近公开记录：")
        for line in recent:
            lines.append("- " + line)
    return "\n" + "\n".join(lines) + "\n"


def _wolves(game):
    out = []
    for seat in game["seats"]:
        if seat["role"] == "werewolf" and seat["alive"]:
            out.append(seat["id"])
    return out


def _wolf_rule(game, night, said):
    # A non-human wolf is a delegated turn: the perform rule asks it, this rule only
    # decides who and with what brief.
    wolves = _wolves(game)
    if len(wolves) == 0:
        game["phase"] = "night_witch"
        return ""
    human = game["player_seat"]
    if game["waiting_for"] == "wolf_target":
        target = _target(said)
        if target <= 0 or not _alive(game, target):
            game["waiting_for"] = "wolf_target"
            return "狼人请重新确认要袭击的座号，例如“刀5号”。"
        night["wolf_proposals"].append({"seat": human, "target": target})
        night["wolf_slot"] = night["wolf_slot"] + 1
        game["waiting_for"] = ""
        return ""
    if night["wolf_slot"] < len(wolves):
        seat = wolves[night["wolf_slot"]]
        if seat == human:
            game["waiting_for"] = "wolf_target"
            mates = []
            for mate in wolves:
                if mate != seat:
                    mates.append(str(mate) + "号" + _name(game, mate))
            return "狼人请睁眼（私密）。你的狼队友：" + "、".join(mates) + "。请选择今晚要袭击的座号，例如“刀5号”。"
        game["brief_seat"] = seat
        game["brief_kind"] = "wolf_speak"
        return ("你是" + str(seat) + "号" + _name(game, seat) + "，身份：狼人。"
                + _context(game)
                + "今晚请简短发言并给出建议，最后一行写 proposal=<座号>。")
    if human in wolves:
        for proposal in night["wolf_proposals"]:
            if proposal["seat"] == human:
                night["wolf_target"] = proposal["target"]
        night["wolf_decided"] = True
        game["phase"] = "night_witch"
        game["waiting_for"] = ""
        return ""
    game["brief_seat"] = wolves[0]
    game["brief_kind"] = "wolf_decide"
    talks = []
    for row in night["wolf_discussion"]:
        talks.append(str(row["seat"]) + "号" + _name(game, row["seat"]) + "：" + row["text"])
    return ("你是狼队最终决策者（" + str(wolves[0]) + "号" + _name(game, wolves[0]) + "）。"
            + _context(game)
            + "\n狼队今晚的提议：\n" + "\n".join(talks)
            + "\n请输出今晚的最终决定，最后一行写 target=<座号>。")


def _witch_rule(game, night, said):
    witch = _role_seat(game, "witch", True)
    if witch <= 0 or (game["witch"]["save_used"] and game["witch"]["poison_used"]):
        game["phase"] = "night_seer"
        game["waiting_for"] = ""
        return ""
    if game["waiting_for"] == "witch_action":
        action, target = _witch_action(said)
        valid = True
        if action == "save":
            valid = ((not game["witch"]["save_used"]) and target > 0 and target != witch
                     and target == night["wolf_target"] and _alive(game, target))
        elif action == "poison":
            valid = (not game["witch"]["poison_used"]) and target > 0 and _alive(game, target)
        elif action != "none":
            valid = False
        if not valid:
            game["waiting_for"] = "witch_action"
            return "女巫请重新决定：救" + str(night["wolf_target"]) + "号 / 毒X号 / 不用药。注意解药只能救当晚被袭击的目标，不能自救。"
        if action == "save":
            game["witch"]["save_used"] = True
            night["witch_save"] = target
        if action == "poison":
            game["witch"]["poison_used"] = True
            night["witch_poison"] = target
        night["witch_decided"] = True
        game["waiting_for"] = ""
        game["phase"] = "night_seer"
        return ""
    if witch == game["player_seat"]:
        game["waiting_for"] = "witch_action"
        return "女巫请睁眼（私密）。今晚被袭击的是" + str(night["wolf_target"]) + "号。解药可用=" + str(not game["witch"]["save_used"]) + "；毒药可用=" + str(not game["witch"]["poison_used"]) + "。请决定：救X号 / 毒X号 / 不用药。"
    game["brief_seat"] = witch
    game["brief_kind"] = "witch"
    return ("你是" + str(witch) + "号女巫。今晚被袭击的是" + str(night["wolf_target"])
            + "号。解药可用=" + str(not game["witch"]["save_used"])
            + "；毒药可用=" + str(not game["witch"]["poison_used"])
            + _context(game)
            + "\n请输出 action=save/poison/none，及 target=<座号>。")


def _seer_rule(game, night, said):
    seer = _role_seat(game, "seer", True)
    if seer <= 0:
        game["phase"] = "dawn"
        game["waiting_for"] = ""
        return ""
    if game["waiting_for"] == "seer_action":
        target = _target(said)
        if target <= 0 or target == seer or not _alive(game, target):
            game["waiting_for"] = "seer_action"
            return "预言家请重新选择查验目标：输入“验X号”，目标必须存活且不能是自己。"
        camp = "狼人阵营" if _role(game, target) == "werewolf" else "好人阵营"
        game["seer_results"].append({"day": game["day"], "seer": seer, "target": target, "camp": camp})
        night["seer_decided"] = True
        game["waiting_for"] = ""
        game["phase"] = "dawn"
        return ""
    if seer == game["player_seat"]:
        game["waiting_for"] = "seer_action"
        return "预言家请睁眼（私密）。请选择今晚查验的目标，例如“验5号”。"
    game["brief_seat"] = seer
    game["brief_kind"] = "seer"
    return ("你是" + str(seer) + "号预言家。"
            + _context(game)
            + "\n请选择今晚查验的目标，最后一行输出 target=<座号>。")


def run(state):
    game = state["game"]
    if game.get("phase") == "night_wolf":
        _night(game)
    night = _night(game)
    said = _last_said(state["speech"], "user")
    prompt = ""
    if game["phase"] == "night_wolf":
        prompt = _wolf_rule(game, night, said)
    if game["phase"] == "night_witch":
        prompt = _witch_rule(game, night, said)
    if game["phase"] == "night_seer":
        prompt = _seer_rule(game, night, said)
    if (game.get("brief_seat") or 0) != 0:
        return {"game": game, "prompt": prompt, "brief": prompt}
    return {"game": game, "prompt": prompt, "brief": ""}
