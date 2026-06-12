(function() {
  config = config || {};
  var nodeID = node.id();
  var keyPrefix = "__approval." + nodeID + ".";
  var decisionKey = keyPrefix + "decision";
  var statusKey = keyPrefix + "status";
  var requestKey = keyPrefix + "request";

  function clearVar(key) {
      if (typeof board.deleteVar === "function") {
          board.deleteVar(key);
      } else {
          board.setVar(key, undefined);
      }
  }

  var decision = board.getVar(decisionKey);
  var status = board.getVar(statusKey);
  var legacyRequest = board.getVar("approval_request");
  var legacyRequestForThisNode = legacyRequest && legacyRequest.node_id === nodeID;
  var legacyRequestForOtherNode = legacyRequest && legacyRequest.node_id && legacyRequest.node_id !== nodeID;

  if (!decision) {
      if (legacyRequestForOtherNode) {
          clearVar("approval_decision");
      } else {
          decision = board.getVar("approval_decision");
      }
  }

  if (decision) {
      board.setVar(statusKey, decision);
      board.setVar("approval_status", decision);
      clearVar(decisionKey);
      clearVar("approval_decision");
      clearVar(requestKey);
      if (!legacyRequestForOtherNode) {
          clearVar("approval_request");
      }
  } else if (status !== "pending") {
      var request = {
          prompt: config.prompt || "Please approve or reject this action.",
          node_id: nodeID,
      };
      board.setVar(statusKey, "pending");
      board.setVar(requestKey, request);
      board.setVar("approval_status", "pending");
      board.setVar("approval_request", request);
      signal.interrupt({ kind: "user_input", message: "waiting for approval" });
  }
})();
