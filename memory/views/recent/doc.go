// Package recent contains derived conversation-context views over MessageLog.
//
// Window is a read-time view over memory/sources/message.Store that returns a
// bounded tail or forward slice of canonical messages. SummaryDAG stores
// rebuildable summary nodes for long-context compression. They are sibling
// views over MessageLog evidence; neither is a short-term memory store or a
// canonical message store.
package recent
