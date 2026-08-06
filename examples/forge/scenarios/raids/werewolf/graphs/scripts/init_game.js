function seededRandom(seed) {
  let h = 2166136261;
  const s = String(seed || "");
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return function() {
    h += 0x6D2B79F5;
    let t = h;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}
function shuffle(list, rand) {
  const out = list.slice();
  for (let i = out.length - 1; i > 0; i--) {
    const j = Math.floor(rand() * (i + 1));
    const tmp = out[i];
    out[i] = out[j];
    out[j] = tmp;
  }
  return out;
}

// Archive the previous finished/in-progress game before dealing a new one.
try {
  const oldRaw = fs.read("game_state.json");
  const old = JSON.parse(oldRaw);
  if (old && old.started === true) {
    fs.write("archive/game_" + Date.now() + ".json", JSON.stringify(old, null, 2));
  }
} catch (e) {
  // no previous game to archive
}

const roleSeed = "werewolf-" + Date.now() + "-" + Math.floor(Math.random() * 1000000);
const roles = shuffle(
  ["werewolf", "werewolf", "werewolf", "seer", "witch", "hunter", "villager", "villager"],
  seededRandom(roleSeed)
);
const baseSeats = [
  { id: 1, name: "林知", persona: "谨慎、会反咬、擅长装好人" },
  { id: 2, name: "阿澈", persona: "逻辑清楚但语气不强势，愿意公开报验" },
  { id: 3, name: "你", persona: "人类玩家" },
  { id: 4, name: "小满", persona: "容易犹豫，重视票型" },
  { id: 5, name: "周岚", persona: "爱打圆场、转移焦点、缓和矛盾" },
  { id: 6, name: "陈医生", persona: "保守，倾向多听一轮" },
  { id: 7, name: "老赵", persona: "直接、强硬、喜欢拍桌归票" },
  { id: 8, name: "苏禾", persona: "安静，容易被忽视，但复盘时会补充细节" }
];
const seats = baseSeats.map(function(s, i) {
  return Object.assign({}, s, { role: roles[i], alive: true, death_reason: "", death_day: 0 });
});
const playerSeat = 3;
const state = {
  started: true,
  phase: "night_wolf",
  waiting_for: "",
  next_rule: "night_wolf",
  day: 1,
  player_seat: playerSeat,
  player_role: seats[playerSeat - 1].role,
  role_seed: roleSeed,
  seats: seats,
  alive: [1, 2, 3, 4, 5, 6, 7, 8],
  public_log: ["第1夜：游戏开始，主持人进入夜间流程。"],
  public_speeches: [],
  public_events: [],
  public_focus: "当前公开焦点：第1夜正在进行。",
  night: {
    started: false, wolf_slot: 0, wolf_proposals: [], wolf_decided: false,
    wolf_target: 0, witch_save: 0, witch_poison: 0, witch_decided: false,
    seer_target: 0, seer_decided: false
  },
  witch: { save_used: false, poison_used: false },
  seer_results: [],
  current_votes: [],
  vote_records: [],
  pk: { candidates: [], mode: "", round: 0 },
  exile_target: 0,
  hunter_pending: 0,
  winner: "",
  last_night_kill: 0,
  last_exile: 0,
  speaker_order: [],
  speech_index: 0,
  seat_memory: {},
  post_game_announced: false
};

// Reset per-turn routing variables and private channels from any prior game.
for (const ch of ["wolf_team_channel", "witch_channel", "seer_channel", "hunter_channel"]) {
  board.setChannel(ch, []);
}
for (let i = 1; i <= 8; i++) {
  board.setChannel("seat_" + i + "_channel", []);
  board.setChannel("seat_" + i + "_vote_channel", []);
}
for (const v of [
  "werewolf_wolf_step", "werewolf_witch_step", "werewolf_seer_step", "werewolf_hunter_step",
  "werewolf_speech_step", "werewolf_vote_step", "werewolf_speaker_seat", "werewolf_vote_seat",
  "werewolf_decider_seat"
]) {
  board.setVar(v, "");
}
board.setVar("werewolf_reset_requested", "false");

function isAlive(s, id) { return (s.alive || []).map(Number).indexOf(Number(id)) >= 0; }
function aliveSeats(s) { return (s.alive || []).map(Number).sort(function(a, b) { return a - b; }); }
function publicView(s) {
  return {
    phase: s.phase || "setup",
    day: s.day || 0,
    player: { seat: s.player_seat || 0 },
    alive: aliveSeats(s),
    winner: s.winner || "",
    public_focus: s.public_focus || "",
    public_log: Array.isArray(s.public_log) ? s.public_log.slice(-8) : []
  };
}
function syncVars(s) {
  board.setVar("werewolf_game_state", s);
  board.setVar("werewolf_phase", s.phase || "");
  board.setVar("werewolf_waiting_for", s.waiting_for || "");
  board.setVar("werewolf_next_rule", s.next_rule || "");
  for (let i = 1; i <= 8; i++) board.setVar("seat_" + i + "_alive", isAlive(s, i) ? "true" : "false");
  board.setVar("werewolf_game_state_text", JSON.stringify(publicView(s), null, 2));
}
syncVars(state);
