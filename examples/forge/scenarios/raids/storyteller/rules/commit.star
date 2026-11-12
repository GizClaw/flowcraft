# The scheduler's line, checked and turned into work.
#
# The model picks from a list the rules wrote. A pick outside that list is not a
# command: it falls back to the first candidate, so a bad line costs a beat instead
# of corrupting the arc.

def _field(line, name):
    at = line.find(name + "=")
    if at < 0:
        return ""
    rest = line[at + len(name) + 1:]
    stop = rest.find(";")
    if stop >= 0:
        rest = rest[:stop]
    return rest.strip()


def run(state):
    story = state["story"]
    plan = state["plan"]
    line = state["choice"]
    candidates = list(plan["candidates"])
    speaker = _field(line, "speaker")
    if speaker not in candidates:
        if len(candidates) > 0:
            speaker = candidates[0]
        else:
            speaker = "narrator"
    guidance = _field(line, "guidance")
    if guidance == "":
        guidance = plan["goal"]
    story["speaker"] = speaker
    story["guidance"] = guidance

    if speaker == "narrator":
        # A cue is a camera direction, never a line to say.
        question = ("当前阶段：" + plan["phase"] + "\n"
                    + "镜头提示：" + guidance + "\n"
                    + "本阶段边界：" + plan["guard"] + "\n"
                    + "最近的对白：\n" + _recent(story) + "\n"
                    + "请写这一拍的旁白。")
        return {"story": story, "brief": "",
                "narrate_messages": [{"role": "user", "text": question}]}

    brief = ("你是" + speaker + "。\n"
             + "当前阶段：" + plan["phase"] + "\n"
             + "本阶段目标：" + plan["goal"] + "\n"
             + "提示：" + guidance + "\n"
             + "最近的对白：\n" + _recent(story))
    return {"story": story, "brief": brief, "narrate_messages": []}


def _recent(story):
    rows = list(story.get("transcript") or [])
    lines = []
    for row in rows[len(rows) - 8:]:
        lines.append(row["speaker"] + "：" + row["text"])
    if len(lines) == 0:
        return "还没有"
    return "\n".join(lines)
