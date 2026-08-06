// Package middleware provides the built-in execution policies for
// tool.Executor. Each constructor returns a tool.Middleware; compose
// them at Executor construction in the order they should observe
// calls (first = outermost).
//
// A typical production chain:
//
//	exec := tool.NewExecutor(registry,
//	    middleware.Recover(),          // panics become IsError results
//	    middleware.Telemetry(),        // spans, metrics, logs
//	    middleware.Concurrency(10),    // fan-out cap
//	    middleware.Timeout(30*time.Second, map[string]time.Duration{
//	        "exec": 2 * time.Minute,   // per-tool override; 0 exempts
//	    }),
//	    middleware.RateLimit(registry),// honors ToolMeta.RateLimit
//	    middleware.Approval(approver, "exec"),
//	    middleware.Audit(sink),
//	)
//
// Every middleware is independently unit-testable and safe for
// concurrent use. Application-specific policies (budgets, tenancy,
// secret resolution) follow the same tool.Middleware shape and slot
// anywhere into the chain — see tool.Middleware for the contract.
//
// Retry is deliberately not a built-in: re-invoking a tool that
// mutates state is unsafe, and ToolMeta.MutatesState exists so a
// future retry policy can make that distinction deliberately.
package middleware
