function latestUserText() {
  const msgs = board.channel(board.MAIN_CHANNEL) || [];
  for (let i = msgs.length - 1; i >= 0; i--) {
    const msg = msgs[i] || {};
    if (msg.role !== "user") continue;
    if (typeof msg.content === "string" && msg.content.trim()) return msg.content.trim();
    const parts = Array.isArray(msg.content && msg.content.parts) ? msg.content.parts : [];
    const text = parts.map(function(p) {
      return p && p.type === "text" && typeof p.text === "string" ? p.text : "";
    }).join("").trim();
    if (text) return text;
  }
  return "";
}

const text = latestUserText();
let kind = "natural";
if (typeof text === "string" && text.trim().indexOf("/") === 0) {
  const cmd = String(text.trim().split(/\s+/)[0] || "").toLowerCase();
  if (cmd === "/reset") kind = "command_reset";
  else if (cmd === "/status") kind = "command_status";
  else kind = "command_other";
}
board.setVar("werewolf_input_kind", kind);
board.setVar("werewolf_input_text", text);
