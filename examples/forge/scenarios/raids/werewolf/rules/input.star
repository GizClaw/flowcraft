# The table's first line.
#
# The deal is seeded from the state, and nothing else writes that seed: this rule
# takes it from the line that starts the game, so "开始狼人杀 7" deals a table that
# can be replayed by typing the same thing again.

def _last_user(channel):
    for index in range(len(channel) - 1, -1, -1):
        if channel[index]["role"] == "user":
            return channel[index]["text"]
    return ""


def run(state):
    game = state["game"]
    seed = state["seed"]
    if game != None and game.get("started"):
        return {"game": game, "seed": seed}
    line = _last_user(state["speech"]).strip()
    if line == "":
        line = "werewolf"
    return {"game": game, "seed": line}
