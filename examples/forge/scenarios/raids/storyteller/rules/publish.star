# One beat of the story, recorded and either continued or paused for the reader.

def _clean(text):
    return text.strip()


def run(state):
    story = state["story"]
    speaker = story.get("speaker") or "narrator"
    said = ""
    if speaker == "narrator":
        said = _clean(state["narrated"])
    else:
        said = _clean(state["spoken"])
    if said == "":
        # An empty beat still advances the count, so a mealy-mouthed answer cannot
        # stall the arc forever.
        said = "……"
    rows = list(story.get("transcript") or [])
    rows.append({"speaker": speaker, "text": said})
    story["transcript"] = rows
    spoken = list(story.get("phase_spoken") or [])
    if speaker not in spoken:
        spoken.append(speaker)
    story["phase_spoken"] = spoken
    story["turn"] = (story.get("turn") or 0) + 1

    phase = story["phases"][story["phase"]]
    # The pause is the handoff beat itself: prepare asked for the narrator because the
    # phase was due to hand back, and this is that narrator's beat.
    due = story.get("handoff_due") == True and speaker == "narrator"
    beats = (story.get("phase_turn") or 0) + 1
    story["phase_turn"] = beats
    limit = (phase.get("handoff_after") or 0) + 2
    if limit > 0 and beats >= limit and not due:
        # Safety net: a beat that repeats a speaker cannot grow the set, and an arc
        # that cannot grow must still hand back rather than spin.
        due = True
    if story["phase"] == "epilogue":
        due = True
    prompt = ""
    if due:
        story["awaiting_user"] = True
        story["next_phase"] = phase.get("next") or story["phase"]
        story["handoff_due"] = False
        story["phase_turn"] = 0
        prompt = ("（旁白暂停。你可以接话，或让某个角色行动；下一拍会进入"
                  + story["next_phase"] + "。）")
    return {"story": story, "prompt": prompt}
