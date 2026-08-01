// Package kanban is a shared task board for multi-agent
// collaboration: agents submit work for other agents, workers claim
// and complete it, and every state change is published as an event.
//
// # The board does not execute anything
//
// This is the one thing to understand about the package. Kanban owns
// card state and the transitions between states. It never runs a task,
// never resolves an agent id, never starts a goroutine on your behalf.
//
// The reason is that "run this agent" is not a decision the board can
// make. Executing an agent needs an engine, a host, tool wiring, and
// per-call options — assembly the application owns. A board that
// insisted on running things would have to know all of it, and would
// then only work for the one wiring it was built against. By staying
// ignorant, one board serves local engines, remote services, and
// humans at the same time; a card claimed by a background worker and a
// card claimed by a person waiting in a UI are the same card.
//
// So the package gives you a queue with a state machine and an event
// stream. Wiring an executor to it is the application's job, and takes
// about a dozen lines:
//
//	k := kanban.New("scope-1", kanban.WithMaxPending(100))
//	defer k.Close()
//
//	go func() {
//	    for card := range k.Watch(ctx, kanban.Filter{Status: kanban.StatusPending}) {
//	        if !k.Claim(card.ID, workerID) {
//	            continue // another worker won the race
//	        }
//	        go func(c *kanban.Card) {
//	            out, err := runAgent(ctx, c.Task)
//	            if err != nil {
//	                k.Fail(c.ID, err.Error())
//	                return
//	            }
//	            k.Done(c.ID, kanban.Result{Output: out})
//	        }(card)
//	    }
//	}()
//
// # Lifecycle
//
//	Pending ──Claim──▶ Claimed ──Done───▶ Done
//	   ▲                  │
//	   │                  ├───Fail───▶ Failed
//	   │                  │
//	   └──Resume── Suspended ◀─Suspend─┘
//
//	Cancel: Pending | Claimed | Suspended ──▶ Cancelled
//
// Every transition is compare-and-swap. [Kanban.Claim] returns true
// for exactly one caller per card, which is what makes several workers
// on one board safe without any coordination between them. A worker
// that claims a card owes the board a terminal state or a suspend.
//
// [Kanban.Suspend] is the state that makes long-running agent work
// expressible. When an engine interrupts to ask a human a question, the
// card is neither running nor finished: holding it claimed looks like a
// hung worker, failing it is untrue. Suspend releases the worker,
// records an opaque resume reference — usually a checkpoint id — and
// leaves the card outstanding. [Kanban.Resume] hands the reference back
// and returns the card to the queue. The board never interprets the
// reference, which is how it stays uninvolved in how execution is
// checkpointed.
//
// Done, Failed and Cancelled are terminal and become eligible for
// eviction under [WithMaxCards] and [WithCardTTL]. Pending and
// suspended cards are never evicted: they are outstanding work, and
// dropping them would lose it.
//
// # Events
//
// [Kanban.Bus] is the only place the board publishes. Each transition
// emits one [CardEvent] on a card-partitioned subject
// ("kanban.card.<id>.done"), so a consumer can follow one card with
// [PatternCard] or the whole board with [PatternAll]. One payload shape
// covers every kind: subscribers switch on [CardEvent.Status] instead
// of maintaining a struct per event.
//
// The default bus is in-memory and owned by the board. [WithBus]
// substitutes a caller-supplied one — typically a bus already shared
// with other subsystems — and ownership stays with the caller, so
// [Kanban.Close] leaves it open.
//
// # Scheduling is elsewhere
//
// Submitting a card later — after a delay, or on a cron expression — is
// not a board concern: any timer can do it, and the board's part is one
// call to [Kanban.Submit]. Keeping schedulers out means this package
// carries no third-party scheduling dependency and no in-memory timer
// state that a restart would silently lose. See sdkx/kanban/scheduler
// for a cron-and-delay scheduler built on the public API.
//
// # On the name Board
//
// This package deliberately has no Board type, even though "board" is
// the natural word for what it holds. [agent.Board] already means
// something quite different — the blackboard of typed channels and
// control variables that an engine mutates during a single run. Two
// types named Board, one of them per-run execution scratch space and
// the other a cross-agent work queue, is a trap for readers and forces
// import aliases wherever both appear. Here the package name carries
// the meaning and [Kanban] is the type.
package kanban
