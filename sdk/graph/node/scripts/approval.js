(function() {
  var decision = board.getVar("approval_decision");
  var status = board.getVar("approval_status");

  if (decision) {
      board.setVar("approval_status", decision);
  } else if (status !== "pending") {
      board.setVar("approval_status", "pending");
      board.setVar("approval_request", {
          prompt: config.prompt || "Please approve or reject this action.",
          node_id: config.__node_id,
      });
      signal.interrupt("waiting for approval");
  }
})();
