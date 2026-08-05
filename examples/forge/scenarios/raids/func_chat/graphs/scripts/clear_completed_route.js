const intent = board.getVar("matched_intent") || "";
const route = board.getVar("function_route") || intent;
const args = board.getVar("matched_args") || {};
const pending = Array.isArray(board.getVar("pending_routes")) ? board.getVar("pending_routes") : [];
const missingTitle = (route === "play_music" || route === "read_story") && !args.title && !args.series && !args.subject;

if (missingTitle) {
  board.setVar("active_route", route);
  board.setVar("active_args", args);
  board.setVar("pending_routes", pending);
  board.setVar("matched_args_json", JSON.stringify(args || {}));
  board.setVar("pending_routes_json", JSON.stringify(pending));
  board.setVar("has_next_route", false);
} else if (pending.length > 0) {
  const next = pending[0] || {};
  const rest = pending.slice(1);
  board.setVar("route_source", "pending");
  board.setVar("matched_intent", next.intent || "");
  board.setVar("matched_args", next.args || {});
  board.setVar("matched_args_json", JSON.stringify(next.args || {}));
  board.setVar("function_route", next.route || "");
  board.setVar("active_route", "");
  board.setVar("active_args", {});
  board.setVar("pending_routes", rest);
  board.setVar("pending_routes_json", JSON.stringify(rest));
  board.setVar("has_next_route", true);
} else {
  board.setVar("active_route", "");
  board.setVar("active_args", {});
  board.setVar("pending_routes", []);
  board.setVar("matched_intent", "");
  board.setVar("matched_args", {});
  board.setVar("matched_args_json", "{}");
  board.setVar("function_route", "");
  board.setVar("pending_routes_json", "[]");
  board.setVar("has_function_intent", false);
  board.setVar("has_next_route", false);
}
