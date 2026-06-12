(function() {
  var files = config.files || [];
  var commands = config.commands || [];

  for (var i = 0; i < files.length; i++) {
      var file = files[i];
      var content = fs.read(file.path);
      var key = file.state_key || file.path;
      board.setVar(key, content);
  }

  for (var j = 0; j < commands.length; j++) {
      var cmd = commands[j];
      var result = shell.exec(cmd.command);
      if (result.exit_code !== 0) {
          var message = result.stderr || ("command failed: " + cmd.command);
          var kind = "internal";
          if (result.exit_code === -1) {
              kind = "not_available";
              if (message.indexOf("empty command") !== -1 || message.indexOf("not allowed") !== -1) {
                  kind = "validation";
              }
          }
          signal.error({
              kind: kind,
              message: message,
              detail: {
                  command: cmd.command,
                  exit_code: result.exit_code,
                  stderr: result.stderr || "",
              }
          });
          return;
      }
      var k = cmd.state_key || cmd.command;
      board.setVar(k, result.stdout);
  }
})();
