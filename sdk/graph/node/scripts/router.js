(function() {
  var routes = config.routes || [];
  var vars = board.getVars();
  var target = "";

  for (var i = 0; i < routes.length; i++) {
      var route = routes[i];
      if (route.condition) {
          var result = expr.eval(route.condition, vars);
          if (result) {
              target = route.target;
              break;
          }
      }
  }

  if (!target && routes.length > 0) {
      for (var j = 0; j < routes.length; j++) {
          if (!routes[j].condition) {
              target = routes[j].target;
              break;
          }
      }
  }

  board.setVar("route_target", target);
})();
