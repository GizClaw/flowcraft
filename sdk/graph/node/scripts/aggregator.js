(function() {
  var inputKeys = config.input_keys || [];
  var mode = config.mode || "array";
  var outputKey = config.output_key || "aggregated";

  var values = [];
  for (var i = 0; i < inputKeys.length; i++) {
      var val = board.getVar(inputKeys[i]);
      if (val !== undefined) {
          values.push(val);
      }
  }

  var result;
  switch (mode) {
      case "array":
          result = values;
          break;
      case "concat":
          result = values.map(String).join(config.separator || "\n");
          break;
      case "map":
          result = {};
          for (var j = 0; j < inputKeys.length; j++) {
              result[inputKeys[j]] = board.getVar(inputKeys[j]);
          }
          break;
      case "last":
          result = values.length > 0 ? values[values.length - 1] : null;
          break;
      default:
          result = values;
  }

  board.setVar(outputKey, result);
})();
