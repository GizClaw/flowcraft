# The blueprint: one arc of the story, phase by phase.
#
# It is data, not prompt text. A phase names what may happen (goal), what must not
# (guard), who may speak, and how long the beat runs before it hands back to the
# reader. Keeping it here is what lets a run explain itself: the transcript says who
# spoke, the blueprint says which list allowed them to.

PHASES = {
    "threshold": {
        "goal": "破庙雨夜，门外异常敲门。建立气氛、角色分歧和是否应门的张力。",
        "guard": "只能建立破庙雨夜和敲门异常，不揭示最终妖身。",
        "allowed": ["narrator", "wukong", "tangseng", "bajie"],
        "first": "narrator",
        "handoff_after": 3,
        "next": "reveal",
    },
    "reveal": {
        "goal": "根据用户介入推进开门、隔门试探或观察，只露出货郎的可疑与篮中异样。",
        "guard": "可以出现黄毛、黑腥水、篮子、脚印；不要说他已经是某个大妖。",
        "allowed": ["narrator", "wukong", "tangseng", "bajie", "monster"],
        "first": "narrator",
        "handoff_after": 4,
        "next": "probe",
    },
    "probe": {
        "goal": "悟空试探货郎，妖怪继续装可怜，唐僧担心误伤，疑点继续累积。",
        "guard": "只做试探，不动手定输赢；妖怪仍不承认身份。",
        "allowed": ["wukong", "monster", "tangseng", "bajie", "narrator"],
        "first": "wukong",
        "handoff_after": 4,
        "next": "pressure",
    },
    "pressure": {
        "goal": "妖怪反咬或诱骗进庙，悟空发现更多破绽，唐僧与八戒形成分歧。",
        "guard": "可以让妖怪狡辩或诱骗；不要引入第二只妖怪；不要离开破庙。",
        "allowed": ["monster", "wukong", "tangseng", "bajie", "narrator"],
        "first": "monster",
        "handoff_after": 4,
        "next": "exposure",
    },
    "exposure": {
        "goal": "关键破绽出现：黄毛、黑腥水、狼爪影子或脚印暴露，悟空确认货郎不是凡人。",
        "guard": "只确认黄毛狼妖伪装，不要扩展到新的主线。",
        "allowed": ["narrator", "wukong", "monster", "tangseng", "bajie"],
        "first": "narrator",
        "handoff_after": 4,
        "next": "clash",
    },
    "clash": {
        "goal": "冲突进入动作段：悟空逼近，妖怪露怯，伪装开始撑不住。",
        "guard": "动作只围绕破庙门口和狼妖伪装；不要跳到新故事。",
        "allowed": ["wukong", "monster", "narrator", "bajie", "tangseng"],
        "first": "wukong",
        "handoff_after": 4,
        "next": "turning",
    },
    "turning": {
        "goal": "妖怪露出狼形但还想逃或求饶，唐僧要求不要滥杀，八戒害怕又嘴硬。",
        "guard": "可以出现狼耳、狼爪、原形；不要新增同伙或幕后山主。",
        "allowed": ["monster", "tangseng", "wukong", "bajie", "narrator"],
        "first": "monster",
        "handoff_after": 4,
        "next": "resolution",
    },
    "resolution": {
        "goal": "收束主冲突：狼妖被悟空制住后逐走，交代为何夜雨敲门，众人各自反应。",
        "guard": "只解决这只狼妖，不打死它，不再抛新敌人或新地点。",
        "allowed": ["narrator", "wukong", "monster", "tangseng", "bajie"],
        "first": "wukong",
        "handoff_after": 4,
        "next": "aftermath",
    },
    "aftermath": {
        "goal": "余波：雨声渐小，确认狼妖已被逐走、没有更多危险，天明继续上路。",
        "guard": "必须确认本折没有更多妖怪；不要改变狼妖未被打死的结局。",
        "allowed": ["narrator", "tangseng", "bajie", "wukong"],
        "first": "narrator",
        "handoff_after": 3,
        "next": "epilogue",
    },
    "epilogue": {
        "goal": "主冲突已结束。旁白只回应用户对本折的追问、复盘因果，或轻轻引下一折。",
        "guard": "尾声禁止新增敌人、反转、同伙、法器或新地点剧情。",
        "allowed": ["narrator"],
        "first": "narrator",
        "handoff_after": 1,
        "next": "epilogue",
    },
}


def _last_user(channel):
    for index in range(len(channel) - 1, -1, -1):
        if channel[index]["role"] == "user":
            return channel[index]["text"]
    return ""


def run(state):
    story = state["story"]
    if story != None and story.get("started"):
        return {"story": story}
    opening = _last_user(state["messages"]).strip()
    if opening == "":
        opening = "我们在破庙里避雨。"
    return {"story": {
        "started": True,
        "phases": PHASES,
        "phase": "threshold",
        "phase_spoken": [],
        "transcript": [],
        "awaiting_user": False,
        "turn": 0,
        "last_user": opening,
        "opening": opening,
    }}
