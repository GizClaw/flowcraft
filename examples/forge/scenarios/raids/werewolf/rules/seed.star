# Deal a new game.
#
# Starlark has no clock and no randomness, so the deal is seeded from the state
# instead of from the machine. That is a feature for this scenario: the same seed
# deals the same roles, so a test can assert a hand and a replay can reproduce one.
#
# The state it writes is the whole game, in the shape the rest of the rules read:
# seats with roles, the alive list, and the phase machine's starting point.

SEATS = [
    {"id": 1, "name": "林知", "persona": "谨慎、会反咬、擅长装好人"},
    {"id": 2, "name": "阿澈", "persona": "逻辑清楚但语气不强势，愿意公开报验"},
    {"id": 3, "name": "你", "persona": "人类玩家"},
    {"id": 4, "name": "小满", "persona": "容易犹豫，重视票型"},
    {"id": 5, "name": "周岚", "persona": "爱打圆场、转移焦点、缓和矛盾"},
    {"id": 6, "name": "陈医生", "persona": "保守，倾向多听一轮"},
    {"id": 7, "name": "老赵", "persona": "直接、强硬、喜欢拍桌归票"},
    {"id": 8, "name": "苏禾", "persona": "安静，容易被忽视，但复盘时会补充细节"},
]

# Eight seats, two wolves: with three, one night kill and one exile leave the
# wolves at parity and the game is over on day one. Two keeps the table playing long
# enough for the day, the vote and the recap to mean something.
ROLES = ["werewolf", "werewolf", "seer", "witch", "hunter",
         "villager", "villager", "villager"]

PLAYER_SEAT = 3

MASK = 0xFFFFFFFFFFFFFFFF


def _state(seed):
    # splitmix64: a few lines, well distributed, and reproducible from any string.
    h = 1469598103934665603
    for ch in seed.elems():
        h = ((h ^ ord(ch)) * 1099511628211) & MASK
    return [h]


def _next(h):
    h[0] = (h[0] + 0x9E3779B97F4A7C15) & MASK
    z = h[0]
    z = ((z ^ (z >> 30)) * 0xBF58476D1CE4E5B9) & MASK
    z = ((z ^ (z >> 27)) * 0x94D049BB133111EB) & MASK
    return (z ^ (z >> 31)) & MASK


def _shuffle(items, h):
    out = list(items)
    for i in range(len(out) - 1, 0, -1):
        j = _next(h) % (i + 1)
        out[i], out[j] = out[j], out[i]
    return out


def run(state):
    seed = state.get("seed")
    if not seed:
        seed = "werewolf"
    h = _state(str(seed))
    roles = _shuffle(ROLES, h)

    seats = []
    for i in range(len(SEATS)):
        seat = dict(SEATS[i])
        seat["role"] = roles[i]
        seat["alive"] = True
        seat["death_reason"] = ""
        seat["death_day"] = 0
        seats.append(seat)

    game = {
        "seed": str(seed),
        "started": True,
        "phase": "night_wolf",
        "private_phases": ["night_wolf", "night_witch", "night_seer"],
        "waiting_for": "",
        "next_rule": "night_wolf",
        "day": 1,
        "player_seat": PLAYER_SEAT,
        "player_role": roles[PLAYER_SEAT - 1],
        "seats": seats,
        "alive": [seat["id"] for seat in seats],
        "public_log": ["第1夜：游戏开始，主持人进入夜间流程。"],
        "public_speeches": [],
        "public_events": [],
        "public_focus": "当前公开焦点：第1夜正在进行。",
        "night": {},
        "witch": {"save_used": False, "poison_used": False},
        "seer_results": [],
        "current_votes": [],
        "vote_records": [],
        "pk": {"candidates": [], "mode": "", "round": 0},
        "exile_target": 0,
        "hunter_pending": 0,
        "hunter_target": None,
        "post_game_turn": 0,
        "winner": "",
        "last_night_kill": 0,
        "last_exile": 0,
        "speaker_order": [],
        "speech_index": 0,
        "brief_seat": 0,
        "brief_kind": "",
        "post_game_announced": False,
    }
    return {"game": game, "prompt": "第1夜开始。你是" + str(PLAYER_SEAT) + "号" + SEATS[PLAYER_SEAT - 1]["name"] + "，身份：" + _role_label(roles[PLAYER_SEAT - 1]) + "。"}


def _role_label(role):
    labels = {"werewolf": "狼人", "seer": "预言家", "witch": "女巫", "hunter": "猎人", "villager": "平民"}
    return labels.get(role, role)
