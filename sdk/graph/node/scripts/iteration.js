(function() {
  var inputKey = config.input_key != null ? config.input_key : "items";
  var items = board.getVar(inputKey) || [];
  var bodyScript = config.body_script || "";
  var results = [];

  for (var i = 0; i < items.length; i++) {
      board.setVar("__iteration_item", items[i]);
      board.setVar("__iteration_index", i);
      board.setVar("__iteration_result", undefined);

      var sig = runtime.execScript(bodyScript, config);

      if (sig) {
          if (sig.Type === "interrupt") {
              board.setVar("iteration_results", results);
              // Forward kind (engine.Cause) / detail through so the
              // child's classification reaches the host instead of
              // collapsing to a bare-message CauseCustom interrupt.
              signal.interrupt({
                  kind: sig.Kind,
                  message: sig.Message,
                  detail: sig.Detail
              });
              return;
          } else if (sig.Type === "error") {
              signal.error({
                  kind: sig.Kind,
                  message: sig.Message,
                  detail: sig.Detail
              });
              return;
          } else if (sig.Type === "done") {
              break;
          }
      }

      var result = board.getVar("__iteration_result");
      if (result !== undefined) {
          results.push(result);
      }
  }

  board.setVar("iteration_results", results);
  board.setVar("__iteration_item", undefined);
  board.setVar("__iteration_index", undefined);
  board.setVar("__iteration_result", undefined);
})();
