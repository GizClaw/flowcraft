(function() {
  var keys = config.keys || ["response"];
  var template = config.template || "";
  var vars = board.getVars();
  var answer = "";

  if (template) {
      answer = template;
      var entries = Object.entries(vars);
      for (var i = 0; i < entries.length; i++) {
          answer = answer.split("{{." + entries[i][0] + "}}").join(String(entries[i][1] != null ? entries[i][1] : ""));
      }
  } else {
      var parts = [];
      for (var j = 0; j < keys.length; j++) {
          var val = board.getVar(keys[j]);
          if (val !== undefined && val !== null && val !== "") {
              parts.push(String(val));
          }
      }
      answer = parts.join("\n");
  }

  board.setVar("answer", answer);

  if (config.stream !== false) {
      stream.emit("answer", answer);
  }
})();
