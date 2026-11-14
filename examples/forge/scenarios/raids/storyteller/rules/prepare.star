# What this beat may contain, and who may speak in it.
#
# The rules answer the question the model must not answer on its own: the phase's
# allowed list, minus whoever already spoke in it. The scheduler then picks from
# that list; it never widens it.

def _last_user(channel):
    for index in range(len(channel) - 1, -1, -1):
        if channel[index]["role"] == "user":
            return channel[index]["text"]
    return ""


def _tail(story, count):
    rows = list(story.get("transcript") or [])
    lines = []
    for row in rows[len(rows) - count:]:
        lines.append(row["speaker"] + "：" + row["text"])
    return "\n".join(lines)


def _handoff_due(phase, spoken):
    # The handoff is a moment, not a count: the set of speakers stops growing when
    # the same one speaks twice, so "spoken + 1" would ask for the handoff on every
    # later beat of the phase and the arc would never leave it.
    handoff = phase.get("handoff_after") or 0
    if handoff <= 0 or "narrator" not in phase["allowed"]:
        return False
    return len(spoken) + 1 >= handoff


def _candidates(phase, spoken, handoff):
    if phase["name"] == "epilogue":
        return ["narrator"]
    if handoff:
        return ["narrator"]
    if len(spoken) == 0 and phase.get("first"):
        return [phase["first"]]
    unused = []
    for speaker in phase["allowed"]:
        if speaker not in spoken:
            unused.append(speaker)
    if len(unused) > 0:
        return unused
    return list(phase["allowed"])


def run(state):
    story = state["story"]
    latest = _last_user(state["messages"]).strip()
    # The arc pauses for the reader; the next line from the reader resumes it at the
    # phase the last beat named.
    if story.get("awaiting_user") and latest != (story.get("last_user") or ""):
        story["last_user"] = latest
        story["awaiting_user"] = False
        following = story.get("next_phase") or ""
        if following != "":
            story["phase"] = following
            story["next_phase"] = ""
        story["phase_spoken"] = []
    key = story["phase"]
    phases = story["phases"]
    if phases.get(key) == None:
        key = "threshold"
    phase = dict(phases[key])
    phase["name"] = key
    spoken = list(story.get("phase_spoken") or [])
    handoff = _handoff_due(phase, spoken)
    candidates = _candidates(phase, spoken, handoff)
    # Bounded values the prompt template reads. The template never slices: a prompt
    # that interpolated the whole transcript would grow every beat.
    story["phase_goal"] = phase["goal"]
    story["phase_guard"] = phase["guard"]
    story["candidates"] = candidates
    story["spoken"] = "、".join(spoken) if len(spoken) > 0 else "还没有"
    story["recent"] = _tail(story, 10) if len(story.get("transcript") or []) > 0 else "还没有"
    story["last_user"] = latest if latest != "" else "（没有）"
    story["handoff_due"] = handoff
    plan = {
        "phase": key,
        "goal": phase["goal"],
        "guard": phase["guard"],
        "candidates": candidates,
        "spoken": spoken,
        "turn": story.get("turn") or 0,
    }
    # The question is the call's one user message: a rule composes text, the model
    # node reads a messages key, and there is no second way to feed a node.
    question = ("阶段：" + key + "（第" + str(plan["turn"]) + "拍）\n"
                + "本阶段目标：" + phase["goal"] + "\n"
                + "本阶段边界：" + phase["guard"] + "\n"
                + "allowed_speakers：" + "、".join(candidates) + "\n"
                + "本阶段已经说过：" + story["spoken"] + "\n"
                + "最近的对白：\n" + story["recent"] + "\n"
                + "读者刚说：" + story["last_user"] + "\n"
                + "请选一位候选，并给出 speaker=<候选>; guidance=<一句中文提示>。")
    return {"story": story, "plan": plan,
            "plan_messages": [{"role": "user", "text": question}]}
