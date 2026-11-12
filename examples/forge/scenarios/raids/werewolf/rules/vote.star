# The vote: every living seat votes once, and the tally decides between a table and
# a PK round.
#
# A vote is private: the seat's brief goes into its own channel, and the tally only
# publishes who left, never who voted for whom.

def _seat(game, id):
    for seat in game["seats"]:
        if seat["id"] == id:
            return seat
    return {"id": id, "name": str(id) + "号", "role": "", "alive": False, "persona": ""}


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


def _recent(game, limit):
    events = list(game.get("public_events") or [])
    if len(events) > limit:
        events = events[len(events) - limit:]
    if len(events) == 0:
        return "暂无"
    return "\n".join(events)


def _speeches(game, limit):
    rows = list(game.get("public_speeches") or [])
    if len(rows) > limit:
        rows = rows[len(rows) - limit:]
    out = []
    for row in rows:
        out.append(str(row["seat"]) + "号" + _name(game, row["seat"]) + "：" + row["text"])
    if len(out) == 0:
        return "暂无"
    return "\n".join(out)


def _seer_view(game, seat):
    rows = []
    for result in game.get("seer_results") or []:
        if result["seer"] == seat:
            rows.append("第" + str(result["day"]) + "夜查验：" + str(result["target"]) + "号" + _name(game, result["target"]) + "为" + result["camp"])
    if len(rows) == 0:
        return "暂无"
    return " | ".join(rows)


def _role_label(role):
    labels = {"werewolf": "狼人", "seer": "预言家", "witch": "女巫", "hunter": "猎人", "villager": "平民"}
    return labels.get(role, role)


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
def _number(text):
    for number in _numbers(text):
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
    # A line that names no target and no abstention is not a vote: the caller keeps
    # the turn open instead of recording it as an abstention nobody chose.
    at = text.find("vote=")
    if at >= 0:
        number = _seat_number(text, at)
        if number:
            return number, True
    at = text.rfind("投")
    if at >= 0:
        number = _seat_number(text, at + 1)
        if number >= 1 and number <= 8:
            return number, True
    if "弃票" in text or "弃权" in text or "不投任何人" in text or "不投票" in text:
        return 0, True
    number = _number(text)
    return number, number != 0


def _vote(text):
    target, _ = _vote_answer(text)
    return target


def _last_said(channel, who):
    for index in range(len(channel) - 1, -1, -1):
        if channel[index]["role"] == who:
            return channel[index]["text"]
    return ""


def _brief(game, voter, pk):
    if pk != None:
        candidates = []
        for id in pk:
            candidates.append(str(id) + "号")
        restriction = "本轮只能投票给PK台上的玩家：" + "、".join(candidates) + "。"
    else:
        # A vote is the table's only lever: abstaining is a real choice but it must
        # not be the default reading of "you may", or a table that is unsure abstains
        # in unison and no one is ever exiled.
        restriction = ("必须投给一名存活玩家。只有在确实无法判断、且不愿跟票时才弃票。"
                       )
    return ("你是" + str(voter) + "号" + _name(game, voter) + "，身份=" + _role_label(_seat(game, voter)["role"]) +
            "。你的查验=" + _seer_view(game, voter) + "。" +
            "\n" + restriction +
            "\n\n公开焦点：" + (game.get("public_focus") or "暂无") +
            "\n\n公开记录（最近事件）：\n" + _recent(game, 15) +
            "\n\n近期发言：\n" + _speeches(game, 14) +
            "\n\n请私密输出你的投票，最后一行：vote=<座号|0>（0=弃票）。")


def _accept(game, pk, target):
    if pk != None:
        return target == 0 or target in pk
    return target == 0 or _alive(game, target)


def _last_user(channel):
    for index in range(len(channel) - 1, -1, -1):
        if channel[index]["role"] == "user":
            return channel[index]["text"]
    return ""


def _pk_speech(game, channel):
    # The tie round: the candidates speak once more, then the table votes again
    # between them. Without this the tally would tie, reopen the PK and tie again.
    pk = game["pk"]
    order = game.get("speaker_order") or pk["candidates"]
    if game.get("waiting_for") == "pk_speech":
        rows = list(game.get("public_speeches") or [])
        rows.append({"seat": game["player_seat"], "day": game["day"], "text": _last_user(channel)})
        game["public_speeches"] = rows
        game["speech_index"] = game["speech_index"] + 1
        game["waiting_for"] = ""
    if game["speech_index"] >= len(order):
        pk["mode"] = "vote"
        game["vote_pending"] = [id for id in game["alive"] if id not in pk["candidates"]]
        game["vote_index"] = 0
        game["current_votes"] = []
        game["public_focus"] = "当前公开焦点：PK 投票，只能投PK台上的候选人。"
        return {"game": game, "prompt": "", "brief": ""}
    seat = order[game["speech_index"]]
    if seat == game["player_seat"]:
        game["waiting_for"] = "pk_speech"
        return {"game": game, "prompt": "你进入了PK台。请再发一轮言为自己拉票。", "brief": ""}
    game["brief_seat"] = seat
    game["brief_kind"] = "speech"
    return {"game": game, "prompt": "",
            "brief": "你进入了PK台，请再发一轮言为自己拉票，最后一行不必写投票。"}


def _tally(game, pk):
    counts = {}
    for vote in game["current_votes"]:
        target = vote["target"]
        if target == 0:
            continue
        counts[target] = (counts.get(target) or 0) + 1
    game["vote_records"] = list(game.get("vote_records") or []) + [{"day": game["day"], "votes": game["current_votes"]}]
    if len(counts) == 0:
        game["exile_target"] = 0
        game["phase"] = "exile"
        game["waiting_for"] = ""
        _log(game, "第" + str(game["day"]) + "天：全员弃票，无人被放逐。")
        return
    best = max(counts.values())
    tied = []
    for target in sorted(counts.keys()):
        if counts[target] == best:
            tied.append(target)
    if len(tied) > 1 and pk == None:
        game["pk"] = {"mode": "speech", "candidates": tied, "round": (game.get("pk") or {}).get("round", 0) + 1}
        game["phase"] = "pk"
        game["speaker_order"] = tied
        game["speech_index"] = 0
        game["waiting_for"] = ""
        names = []
        for id in tied:
            names.append(str(id) + "号" + _name(game, id))
        _log(game, "投票平票，进入PK：候选人为" + "、".join(names) + "。")
        _event(game, "投票平票，进入PK：候选人为" + "、".join(names) + "。")
        return
    if len(tied) > 1:
        # A tied PK still has to leave someone out: the table settles it by seat order.
        tied = [tied[0]]
    game["exile_target"] = tied[0]
    game["phase"] = "exile"
    game["waiting_for"] = ""
    _event(game, "第" + str(game["day"]) + "天投票结果：" + str(tied[0]) + "号" + _name(game, tied[0]) + "票数最高。")


def run(state):
    game = state["game"]
    pk = (game.get("pk") or {}).get("candidates") if (game.get("pk") or {}).get("mode") == "vote" else None
    if pk != None and len(pk) == 0:
        pk = None
    voters = game.get("vote_pending") or game["alive"]
    prompt = ""

    if game["phase"] == "pk" and (game.get("pk") or {}).get("mode") == "speech":
        return _pk_speech(game, state["speech"])

    if game["phase"] == "tally":
        _tally(game, pk)
        return {"game": game, "prompt": "", "brief": ""}

    if game["waiting_for"] == "vote" or game["waiting_for"] == "pk_vote":
        target, sure = _vote_answer(_last_said(state["speech"], "user"))
        if (not sure) or (not _accept(game, pk, target)):
            game["waiting_for"] = "vote" if pk == None else "pk_vote"
            return {"game": game, "prompt": "请明确投票座号或弃票，例如“我投5号”或“弃票”。", "brief": ""}
        game["current_votes"] = list(game.get("current_votes") or []) + [{"voter": game["player_seat"], "target": target}]
        game["vote_index"] = game["vote_index"] + 1
        game["waiting_for"] = ""

    if game["vote_index"] >= len(voters):
        game["phase"] = "tally"
        game["waiting_for"] = ""
        return {"game": game, "prompt": "", "brief": ""}

    voter = voters[game["vote_index"]]
    if voter == game["player_seat"]:
        game["waiting_for"] = "vote" if pk == None else "pk_vote"
        return {"game": game, "prompt": "请私密投票，输入“我投X号”或“弃票”。", "brief": ""}
    game["brief_seat"] = voter
    game["brief_kind"] = "vote"
    return {"game": game, "prompt": "", "brief": _brief(game, voter, pk)}
