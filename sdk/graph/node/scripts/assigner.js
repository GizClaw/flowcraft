(function() {
  var assignments = config.assignments || [];

  for (var i = 0; i < assignments.length; i++) {
      var assign = assignments[i];
      var value;
      if (assign.source !== undefined) {
          value = board.getVar(assign.source);
      } else if (assign.value !== undefined) {
          value = assign.value;
      } else if (assign.expression !== undefined) {
          value = expr.eval(assign.expression, board.getVars());
      }

      var target = assign.target;
      if (target.indexOf(".") !== -1) {
          var parts = target.split(".");
          var obj = board.getVar(parts[0]) || {};
          var current = obj;
          for (var j = 1; j < parts.length - 1; j++) {
              if (current[parts[j]] === undefined) {
                  current[parts[j]] = {};
              }
              current = current[parts[j]];
          }
          current[parts[parts.length - 1]] = value;
          board.setVar(parts[0], obj);
      } else {
          board.setVar(target, value);
      }
  }
})();
