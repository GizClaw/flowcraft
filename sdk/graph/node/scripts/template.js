(function() {
  var template = config.template || "";
  var vars = board.getVars();
  var output = template;

  var entries = Object.entries(vars);
  for (var i = 0; i < entries.length; i++) {
      var placeholder = "{{." + entries[i][0] + "}}";
      if (output.indexOf(placeholder) !== -1) {
          output = output.split(placeholder).join(String(entries[i][1] != null ? entries[i][1] : ""));
      }
  }

  board.setVar("template_output", output);
})();
