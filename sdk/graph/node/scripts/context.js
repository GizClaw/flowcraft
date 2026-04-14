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
      var k = cmd.state_key || cmd.command;
      board.setVar(k, result.stdout);
  }
})();
