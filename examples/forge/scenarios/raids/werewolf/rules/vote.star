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


def _vote(text):
    if "弃票" in text or "不投" in text:
        return 0
    at = text.find("vote=")
    if at >= 0:
        numbers = _numbers(text[at:])
        if numbers:
            return numbers[0]
    return _number(text)


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
        restriction = "可以投任意存活玩家，也可以弃票。"
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
    channels = {"1": state["seat_1"], "2": state["seat_2"], "3": state["seat_3"], "4": state["seat_4"],
                "5": state["seat_5"], "6": state["seat_6"], "7": state["seat_7"], "8": state["seat_8"]}
    pk = (game.get("pk") or {}).get("candidates") if (game.get("pk") or {}).get("mode") == "vote" else None
    if pk != None and len(pk) == 0:
        pk = None
    voters = game.get("vote_pending") or game["alive"]
    prompt = ""

    if game["phase"] == "tally":
        _tally(game, pk)
        return {"game": game, "prompt": ""}

    if game["waiting_for"] == "vote" or game["waiting_for"] == "pk_vote":
        target = _vote(_last_said(state["speech"], "user"))
        if not _accept(game, pk, target):
            game["waiting_for"] = "vote" if pk == None else "pk_vote"
            return {"game": game, "prompt": "请明确投票座号或弃票，例如“我投5号”或“弃票”。"}
        game["current_votes"] = list(game.get("current_votes") or []) + [{"voter": game["player_seat"], "target": target}]
        game["vote_index"] = game["vote_index"] + 1
        game["waiting_for"] = ""

    if game.get("vote_step") == "decide":
        seat = game.get("vote_seat") or 0
        target = _vote(_last_said(channels.get(str(seat), []), "assistant"))
        if not _accept(game, pk, target):
            target = 0
        game["current_votes"] = list(game.get("current_votes") or []) + [{"voter": seat, "target": target}]
        game["vote_index"] = game["vote_index"] + 1
        game["vote_step"] = ""
        game["vote_seat"] = 0

    if game["vote_index"] >= len(voters):
        game["phase"] = "tally"
        game["waiting_for"] = ""
        return {"game": game, "prompt": ""}

    voter = voters[game["vote_index"]]
    target_channel = ""
    if voter == game["player_seat"]:
        game["waiting_for"] = "vote" if pk == None else "pk_vote"
        prompt = "请私密投票，输入“我投X号”或“弃票”。"
    else:
        game["vote_seat"] = voter
        game["vote_step"] = "decide"
        target_channel = "seat_" + str(voter)
        prompt = _brief(game, voter, pk)

    writes = {"game": game, "prompt": prompt}
    if target_channel != "":
        writes[target_channel] = prompt
    return writes
