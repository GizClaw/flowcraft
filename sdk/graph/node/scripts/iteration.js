(function() {
  var inputKey = config.input_key != null ? config.input_key : "items";
  var items = board.getVar(inputKey) || [];
  var bodyScript = config.body_script || "";
  var results = [];

  for (var i = 0; i < items.length; i++) {
      board.setVar("__iteration_item", items[i]);
      board.setVar("__iteration_index", i);

      runtime.execScript(bodyScript, config, []);

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
