parallel.cancelNode("chat_buffer", "function route selected");

function latestUserText() {
  const msgs = board.channel(board.MAIN_CHANNEL) || [];
  for (let i = msgs.length - 1; i >= 0; i--) {
    const msg = msgs[i] || {};
    if (msg.role !== "user") continue;
    const parts = Array.isArray(msg.content.parts) ? msg.content.parts : [];
    const text = parts.map(function(part) {
      return part && part.type === "text" && typeof part.text === "string" ? part.text : "";
    }).join("").trim();
    if (text) return text;
  }
  return "";
}

const intent = String(board.getVar("matched_intent") || "");
const route = String(board.getVar("function_route") || intent || "");
const args = board.getVar("matched_args") || {};
const pending = Array.isArray(board.getVar("pending_routes")) ? board.getVar("pending_routes") : [];
const argsJSON = JSON.stringify(args || {});
const pendingJSON = JSON.stringify(pending);
board.setVar("matched_args_json", argsJSON);
board.setVar("pending_routes_json", pendingJSON);

const context = [
  "You are a function node in a routed assistant graph.",
  "Current route context:",
  "- intent: " + intent,
  "- route: " + route,
  "- matched_args_json: " + argsJSON,
  "- pending_routes_json: " + pendingJSON,
  "Use matched_args_json as the source of truth for the current function. Ignore earlier conversation, previous function output, and tool results if they conflict with this context.",
  "Complete only the current function. Do not mention or execute pending routes."
];
board.setVar("function_context", context.join("\n"));
board.setChannel("function_route_channel", [{
  role: "user",
  content: { parts: [{ type: "text", text: latestUserText() }] },
}]);
