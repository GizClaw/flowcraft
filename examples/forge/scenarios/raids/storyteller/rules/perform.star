# One character's line: whoever the scheduler picked, asked through delegation.

def run(state):
    story = state["story"]
    seat = story.get("speaker") or ""
    brief = state["brief"]
    if seat == "" or seat == "narrator" or brief == "":
        return {"spoken": ""}
    answer = tool("delegate", {
        "target": "seat_" + seat,
        "input": "story=" + str(story.get("opening") or "") + "\nkind=line\n" + brief,
    })
    return {"spoken": answer["text"]}
