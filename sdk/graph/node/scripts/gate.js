(function() {
  var commands = config.commands || [];
  var passed = true;
  var output = "";
  var failedCommand = "";

  for (var i = 0; i < commands.length; i++) {
      var cmd = commands[i];
      var result = shell.exec(cmd);
      output += result.stdout;
      if (result.exit_code !== 0) {
          passed = false;
          failedCommand = cmd;
          break;
      }
  }

  board.setVar("gate_result", passed ? "pass" : "fail");
  board.setVar("gate_result_output", output);
  if (failedCommand) {
      board.setVar("gate_result_failed_command", failedCommand);
  }

  if (!passed) {
      signal.done();
  }
})();
