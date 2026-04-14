(function() {
  var maxCount = config.max_count != null ? config.max_count : 10;
  var counterKey = config.counter_key != null ? config.counter_key : "__loop_count";
  var count = (board.getVar(counterKey) || 0) + 1;
  board.setVar(counterKey, count);
  board.setVar("loop_count", count);
  board.setVar("loop_count_exceeded", count >= maxCount);
})();
