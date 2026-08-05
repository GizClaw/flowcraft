// Publish the function branch output to the main channel (the
// legacy graph did this with publish: true on the llm node).
var routeMsgs = board.channel("function_route_channel") || [];
var lastAssistant = null;
for (var i = routeMsgs.length - 1; i >= 0; i--) {
  var m = routeMsgs[i] || {};
  if (m.role === "assistant") { lastAssistant = m; break; }
}
if (lastAssistant) {
  board.appendChannel(board.MAIN_"function_route_channel", lastAssistant);
}