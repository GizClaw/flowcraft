# The day: every living seat speaks once, in seat order, and then the table votes.
#
# The rule decides; it does not speak. A seat's line is fetched by the perform rule
# through the delegate tool, which is also where its private memory is injected.

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


def _say(game, seat, text):
    rows = list(game.get("public_speeches") or [])
    rows.append({"seat": seat, "day": game["day"], "text": text})
    game["public_speeches"] = rows


def _seer_view(game, seat):
    rows = []
    for result in game.get("seer_results") or []:
        if result["seer"] == seat:
            rows.append("第" + str(result["day"]) + "夜查验：" + str(result["target"]) + "号" + _name(game, result["target"]) + "为" + result["camp"])
    if len(rows) == 0:
        return "暂无"
    return " | ".join(rows)


def _brief(game, seat, extra):
    role = _seat(game, seat)["role"]
    return ("发言人=" + str(seat) + "号" + _name(game, seat) + "；你的身份=" + _role_label(role) +
            "；风格=" + (_seat(game, seat)["persona"] or "普通") +
            "\n你的查验=" + _seer_view(game, seat) +
            "；只能使用自己的身份、自己的查验和公开信息，不得知道别人的隐藏身份。" +
            "\n\n当前公开焦点：" + (game.get("public_focus") or "暂无") +
            "\n\n公开记录（最近事件）：\n" + _recent(game, 15) +
            "\n\n近期公开发言：\n" + _speeches(game, 14) +
            extra)


def _role_label(role):
    labels = {"werewolf": "狼人", "seer": "预言家", "witch": "女巫", "hunter": "猎人", "villager": "平民"}
    return labels.get(role, role)


def _last_said(channel, who):
    for index in range(len(channel) - 1, -1, -1):
        if channel[index]["role"] == who:
            return channel[index]["text"]
    return ""


def run(state):
    game = state["game"]
    order = game.get("speaker_order") or game["alive"]
    prompt = ""

    if game["waiting_for"] == "day_speech":
        _say(game, game["player_seat"], _last_said(state["speech"], "user"))
        game["speech_index"] = game["speech_index"] + 1
        game["waiting_for"] = ""

    if game["speech_index"] >= len(order):
        game["phase"] = "vote"
        game["vote_index"] = 0
        game["vote_pending"] = game["alive"]
        game["current_votes"] = []
        game["waiting_for"] = ""
        game["public_focus"] = "当前公开焦点：第" + str(game["day"]) + "天投票阶段。"
        return {"game": game, "prompt": ""}

    seat = order[game["speech_index"]]
    if seat == game["player_seat"]:
        game["waiting_for"] = "day_speech"
        prompt = "轮到你（" + str(seat) + "号）发言。请说明你信谁、怀疑谁，以及理由。"
    else:
        # A seat's turn is a delegate call the perform rule makes; the decision rule
        # only says who and why.
        game["brief_seat"] = seat
        game["brief_kind"] = "speech"
        return {"game": game, "prompt": "", "brief": _brief(game, seat, "")}
    if (game.get("brief_seat") or 0) != 0:
        return {"game": game, "prompt": prompt, "brief": prompt}
    return {"game": game, "prompt": prompt, "brief": ""}
