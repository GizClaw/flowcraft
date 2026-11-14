# The referee's router.
#
# The routing lives in the document's edges, because that is where a condition over
# the state belongs. This node exists so those edges have a source: it reads the game
# and writes it back unchanged, so every rule can return here and the next phase is
# chosen by the same table.
#
# It also answers one question the edges cannot: does the run resume? A human turn
# ends the run so the table can answer; the answer arrives as a new user line in the
# speech channel, so a longer channel than the one the wait was taken at means the
# answer is in. Without this the edges would see the same waiting_for forever and end
# every later run before a rule could consume the answer.

def run(state):
    game = state["game"]
    if game != None and game.get("started"):
        turns = len(state["speech"])
        waiting = game.get("waiting_for") or ""
        game["human_ready"] = waiting != "" and turns > (game.get("speech_len") or 0)
        game["speech_len"] = turns
    return {"game": game}
