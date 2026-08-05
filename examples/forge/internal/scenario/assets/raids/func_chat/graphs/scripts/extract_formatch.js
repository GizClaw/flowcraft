var raw = "";
var formatMsgs = board.channel("route_format_channel") || [];
for (var i = formatMsgs.length - 1; i >= 0; i--) {
  var fm = formatMsgs[i] || {};
  if (fm.role !== "assistant") continue;
  var partsArr =
    fm.content && Array.isArray(fm.content.parts) ? fm.content.parts : [];
  for (var j = partsArr.length - 1; j >= 0; j--) {
    var fpart = partsArr[j] || {};
    if (
      fpart.type === "text" &&
      typeof fpart.text === "string" &&
      fpart.text.trim()
    ) {
      raw = fpart.text.trim();
      break;
    }
  }
  if (raw) break;
}
raw = String(raw || "").trim();
const lines = raw
  .split(/\r?\n/)
  .map(function (line) {
    return line.trim();
  })
  .filter(Boolean);

function parseArgs(text) {
  const args = {};
  text.split(",").forEach(function (part) {
    const idx = part.indexOf("=");
    if (idx < 0) return;
    const key = part.slice(0, idx).trim();
    const value = part.slice(idx + 1).trim();
    if (key) args[key] = value;
  });
  return args;
}

const allowed = {
  play_song: true,
  sing: true,
  play_music: true,
  read_story: true,
  adjust_device_volume: true,
  stop_playing: true,
  stop_chat: true,
};

function routeFromIntent(intent) {
  let functionRoute = intent;
  if (intent === "play_song" || intent === "sing") functionRoute = "play_music";
  if (intent === "stop_playing" || intent === "stop_chat")
    functionRoute = "stop";
  return functionRoute;
}

function parseRoute(line) {
  if (line === "nothing") return null;
  const arrowIdx = line.indexOf("->");
  const normalized = arrowIdx >= 0 ? line.slice(arrowIdx + 2).trim() : line;
  const idx = normalized.indexOf(":");
  let intent = "";
  let args = {};
  if (idx < 0) {
    intent = normalized.trim();
  } else {
    intent = normalized.slice(0, idx).trim();
    args = parseArgs(normalized.slice(idx + 1));
  }
  if (!allowed[intent]) return null;
  return {
    intent: intent,
    route: routeFromIntent(intent),
    args: args,
  };
}

const routes = [];
for (const line of lines) {
  const parsed = parseRoute(line);
  if (parsed) routes.push(parsed);
}

if (routes.length === 0) {
  board.setVar("route_source", "chat");
  board.setVar("has_function_intent", false);
  board.setVar("matched_intent", "");
  board.setVar("matched_args", {});
  board.setVar("matched_args_json", "{}");
  board.setVar("function_route", "");
  board.setVar("pending_routes", []);
  board.setVar("pending_routes_json", "[]");
  board.setVar("has_next_route", false);
  board.setVar("needs_format", false);
} else {
  const current = routes[0];
  const pending = routes.slice(1);
  board.setVar("has_function_intent", true);
  board.setVar("route_source", "format");
  board.setVar("matched_intent", current.intent);
  board.setVar("matched_args", current.args || {});
  board.setVar("matched_args_json", JSON.stringify(current.args || {}));
  board.setVar("function_route", current.route);
  board.setVar("pending_routes", pending);
  board.setVar("pending_routes_json", JSON.stringify(pending));
  board.setVar("has_next_route", pending.length > 0);
}

const originalMain = board.getVar("tmp_main_channel_before_format");
if (Array.isArray(originalMain)) {
  board.setChannel(board.MAIN_CHANNEL, originalMain);
}
