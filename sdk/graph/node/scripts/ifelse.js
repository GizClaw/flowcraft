(function() {
  var conditions = config.conditions || [];
  var vars = board.getVars();
  var result = "else";

  for (var i = 0; i < conditions.length; i++) {
      var cond = conditions[i];
      var matched = expr.eval(cond.expression, vars);
      if (matched) {
          result = i === 0 ? "if" : "elif_" + i;
          break;
      }
  }

  board.setVar("branch_result", result);
})();
