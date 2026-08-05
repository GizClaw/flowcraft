function latestUserText() {
  const msgs = board.channel(board.MAIN_CHANNEL) || [];
  for (let i = msgs.length - 1; i >= 0; i--) {
    const msg = msgs[i] || {};
    if (msg.role !== "user") continue;
    const parts = Array.isArray(msg.content.parts) ? msg.content.parts : [];
    const text = parts
      .map(function (part) {
        return part && part.type === "text" && typeof part.text === "string"
          ? part.text
          : "";
      })
      .join("")
      .trim();
    if (text) return text;
  }
  return "";
}

const ws = board.getVar("workspace_state") || {};
const activeRoute = board.getVar("active_route") || ws.active_route || "";
const activeArgs = board.getVar("active_args") || ws.active_args || {};
const pendingRoutes = board.getVar("pending_routes") || ws.pending_routes || [];

if (activeRoute) {
  const args = Object.assign({}, activeArgs || {});
  const latest = latestUserText();
  if (
    (activeRoute === "play_music" ||
      activeRoute === "play_song" ||
      activeRoute === "sing") &&
    !args.title &&
    latest
  ) {
    args.title = latest;
  }
  if (
    activeRoute === "read_story" &&
    !args.title &&
    !args.series &&
    !args.subject &&
    latest
  ) {
    args.subject = latest;
  }
  board.setVar("route_source", "state");
  board.setVar("matched_intent", activeRoute);
  board.setVar("matched_args", args);
  board.setVar("matched_args_json", JSON.stringify(args || {}));
  board.setVar("function_route", activeRoute);
  board.setVar(
    "pending_routes",
    Array.isArray(pendingRoutes) ? pendingRoutes : [],
  );
  board.setVar(
    "pending_routes_json",
    JSON.stringify(Array.isArray(pendingRoutes) ? pendingRoutes : []),
  );
  board.setVar("has_next_route", false);
  board.setVar("has_function_intent", true);
  board.setVar("needs_format", false);
} else {
  board.setVar("route_source", "format");
  board.setVar("matched_intent", "");
  board.setVar("matched_args", {});
  board.setVar("matched_args_json", "{}");
  board.setVar("function_route", "");
  board.setVar("pending_routes", []);
  board.setVar("pending_routes_json", "[]");
  board.setVar("has_next_route", false);
  board.setVar("has_function_intent", false);
  board.setVar("needs_format", true);
  board.setVar(
    "tmp_main_channel_before_format",
    board.channel(board.MAIN_CHANNEL),
  );
  board.setChannel("route_format_channel", [
    {
      role: "user",
      content: { parts: [{ type: "text", text: latestUserText() }] },
    },
  ]);
}
