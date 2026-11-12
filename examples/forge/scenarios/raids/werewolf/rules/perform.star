# Perform one seat's turn: whoever the decision rules picked, ask it and record what
# it said.
#
# The referee makes no model calls of its own. A seat is another agent, reached
# through the delegate tool: the call is deterministic (the rules chose the seat and
# wrote the brief), it blocks until the seat answers, and the seat's own stream
# reaches the host through the manager's event hook - which is where the table sees
# it, labelled.

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


_NUMERAL_ORDER = ["一", "二", "两", "三", "四", "五", "六", "七", "八", "九", "十"]


_NUMERALS = {"一": 1, "二": 2, "两": 2, "三": 3, "四": 4, "五": 5,
             "六": 6, "七": 7, "八": 8, "九": 9, "十": 10}


def _seat_number(text, start):
    # Seats answer in Chinese: "我投六号" is as common as "我投6号". A Starlark
    # string is a byte string - indexing it or calling elems() walks bytes, so a
    # numeral is found by substring search, never by comparing one position.
    rest = text[start:]
    numbers = _numbers(rest)
    if numbers:
        return numbers[0]
    earliest = -1
    found = 0
    for name in _NUMERAL_ORDER:
        at = rest.find(name)
        if at >= 0 and (earliest < 0 or at < earliest):
            earliest = at
            found = _NUMERALS[name]
    return found
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


def _vote_answer(text):
    # The brief asks for a machine line; seats answer in prose. A parse that guesses
    # from any number in the text counts the speaker's own seat as often as the
    # target, so guessing is reported as guessing and the caller re-asks.
    # An explicit target beats a hedge: "我投六号，但不行就弃票" is a vote for six,
    # and the brief's own contract puts the machine line last.
    at = text.find("vote=")
    if at >= 0:
        number = _seat_number(text, at)
        if number:
            return number, True
    # The keyword sits mid-sentence as often as at the start: "我这票投2号".
    at = text.rfind("投")
    if at >= 0:
        number = _seat_number(text, at + 1)
        if number >= 1 and number <= 8:
            return number, True
    if "弃票" in text or "弃权" in text or "不投任何人" in text or "不投票" in text:
        return 0, True
    return 0, False


def _witch(text):
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


def run(state):
    game = state["game"]
    seat = game.get("brief_seat") or 0
    kind = game.get("brief_kind") or ""
    brief = state["brief"]
    game["brief_seat"] = 0
    game["brief_kind"] = ""
    if seat == 0 or brief == "":
        return {"game": game, "brief": "", "spoken": ""}

    # The seat's turn: the game id rides the brief so its memory hook can key on it.
    answer = tool("delegate", {
        "target": "seat_" + str(seat),
        "input": "game=" + str(game.get("seed") or "") + "\n" + brief,
    })
    said = answer["text"]

    if kind == "speech":
        rows = list(game.get("public_speeches") or [])
        rows.append({"seat": seat, "day": game["day"], "text": said})
        game["public_speeches"] = rows
        game["speech_index"] = game["speech_index"] + 1
        _event(game, "第" + str(game["day"]) + "天：" + str(seat) + "号" + _name(game, seat) + "：" + said)
    if kind == "vote":
        target, sure = _vote_answer(said)
        if not sure:
            # One re-ask, with the contract spelled out: a seat that answered the
            # question in prose gets one chance to answer it in the one line the
            # rules can read before the vote is recorded as an abstention.
            alive = []
            for id in game["alive"]:
                alive.append(str(id))
            again = tool("delegate", {
                "target": "seat_" + str(seat),
                "input": "game=" + str(game.get("seed") or "")
                         + "\n你上一轮的回答没有给出投票。只回复一行：vote=<座号|0>（0=弃票）。"
                         + "\n候选座号：" + "、".join(alive)
                         + "\n你上一轮的回答：" + said,
            })
            said = again["text"]
            target, sure = _vote_answer(said)
        if target != 0 and target not in game["alive"]:
            target = 0
            sure = False
        if not sure:
            _event(game, str(seat) + "号" + _name(game, seat) + "的回答没有可解析的投票，记为弃票。")
        game["current_votes"] = list(game.get("current_votes") or []) + [{"voter": seat, "target": target}]
        game["vote_index"] = game["vote_index"] + 1
    if kind == "wolf_speak":
        night = game.get("night") or {}
        night["wolf_proposals"] = list(night.get("wolf_proposals") or []) + [{"seat": seat, "target": _target(said)}]
        night["wolf_discussion"] = list(night.get("wolf_discussion") or []) + [{"seat": seat, "text": said}]
        night["wolf_slot"] = night["wolf_slot"] + 1
        game["night"] = night
    if kind == "wolf_decide":
        night = game.get("night") or {}
        target = _target(said)
        if target == 0 or target not in game["alive"]:
            target = game["alive"][0]
        night["wolf_target"] = target
        night["wolf_decided"] = True
        game["night"] = night
        game["phase"] = "night_witch"
    if kind == "witch":
        night = game.get("night") or {}
        action, target = _witch(said)
        if action == "save" and not game["witch"]["save_used"] and target == night.get("wolf_target"):
            game["witch"]["save_used"] = True
            night["witch_save"] = target
        if action == "poison" and target in game["alive"]:
            game["witch"]["poison_used"] = True
            night["witch_poison"] = target
        night["witch_decided"] = True
        game["night"] = night
        game["phase"] = "night_seer"
    if kind == "hunter":
        game["hunter_target"] = _target(said)
    if kind == "postgame":
        rows = list(game.get("public_speeches") or [])
        rows.append({"seat": seat, "day": game["day"], "text": said})
        game["public_speeches"] = rows
        _event(game, "复盘：" + str(seat) + "号" + _name(game, seat) + "：" + said)
    if kind == "seer":
        night = game.get("night") or {}
        target = _target(said)
        if target != 0 and target in game["alive"]:
            camp = "狼人阵营" if _role(game, target) == "werewolf" else "好人阵营"
            game["seer_results"] = list(game.get("seer_results") or []) + [
                {"day": game["day"], "seer": seat, "target": target, "camp": camp}]
        night["seer_decided"] = True
        game["night"] = night
        game["phase"] = "dawn"
    return {"game": game, "brief": "", "spoken": said}


def _role(game, id):
    for seat in game["seats"]:
        if seat["id"] == id:
            return seat["role"]
    return ""
