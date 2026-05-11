package taubench

import (
	"fmt"
	"strings"
)

// NewRetailTools returns the canonical retail-domain tool registry.
// The shape mirrors τ-bench's retail dataset (orders, products,
// customers) but with a deliberately small surface: enough variety
// that an agent needs to chain tool calls, but few enough that we can
// curate fixtures inline without dragging in upstream JSON.
//
// Tools (all read-or-write a single State value):
//
//	get_order(order_id)                         → order record or {error}
//	list_orders_for_customer(customer_id)       → []order_id
//	cancel_order(order_id, reason)              → updates orders.<id>.status to "cancelled"
//	update_shipping(order_id, address)          → updates orders.<id>.shipping_address
//	get_product(product_id)                     → product record or {error}
//	search_products(query)                      → []product_id whose name/description contains query (case-insensitive)
//
// Every handler returns a JSON-encodable value and never panics on
// missing keys (returns an "error" payload instead) so the agent
// receives an actionable error message rather than a tool-execution
// crash.
func NewRetailTools() map[string]Tool {
	return map[string]Tool{
		"get_order": {
			Name:        "get_order",
			Description: "Look up an order by its id. Returns the full order record (status, items, shipping_address, customer_id) or an error if the id is unknown.",
			InputSchema: objectSchema(map[string]any{
				"order_id": stringSchema("Order id, e.g. ORD-1001"),
			}, []string{"order_id"}),
			Handler: func(state State, args map[string]any) (any, error) {
				id, _ := args["order_id"].(string)
				orders, _ := state["orders"].(map[string]any)
				rec, ok := orders[id].(map[string]any)
				if !ok {
					return map[string]any{"error": fmt.Sprintf("order %q not found", id)}, nil
				}
				return rec, nil
			},
		},
		"list_orders_for_customer": {
			Name:        "list_orders_for_customer",
			Description: "List every order id belonging to a customer.",
			InputSchema: objectSchema(map[string]any{
				"customer_id": stringSchema("Customer id, e.g. CUST-7"),
			}, []string{"customer_id"}),
			Handler: func(state State, args map[string]any) (any, error) {
				cid, _ := args["customer_id"].(string)
				orders, _ := state["orders"].(map[string]any)
				var hits []string
				for id, rec := range orders {
					if m, ok := rec.(map[string]any); ok && m["customer_id"] == cid {
						hits = append(hits, id)
					}
				}
				return map[string]any{"order_ids": hits}, nil
			},
		},
		"cancel_order": {
			Name:        "cancel_order",
			Description: "Cancel an order. Sets status to \"cancelled\" and records the supplied reason. Only orders in status \"pending\" or \"processing\" can be cancelled.",
			InputSchema: objectSchema(map[string]any{
				"order_id": stringSchema("Order id to cancel"),
				"reason":   stringSchema("Free-text reason supplied by the customer"),
			}, []string{"order_id", "reason"}),
			Handler: func(state State, args map[string]any) (any, error) {
				id, _ := args["order_id"].(string)
				reason, _ := args["reason"].(string)
				orders, _ := state["orders"].(map[string]any)
				rec, ok := orders[id].(map[string]any)
				if !ok {
					return map[string]any{"error": fmt.Sprintf("order %q not found", id)}, nil
				}
				status, _ := rec["status"].(string)
				if status != "pending" && status != "processing" {
					return map[string]any{"error": fmt.Sprintf("order %q is in status %q and cannot be cancelled", id, status)}, nil
				}
				rec["status"] = "cancelled"
				rec["cancellation_reason"] = reason
				return map[string]any{"order_id": id, "status": "cancelled"}, nil
			},
		},
		"update_shipping": {
			Name:        "update_shipping",
			Description: "Replace the shipping address on an order. Only orders in status \"pending\" can have shipping addresses changed.",
			InputSchema: objectSchema(map[string]any{
				"order_id": stringSchema("Order id"),
				"address":  stringSchema("New shipping address, single string"),
			}, []string{"order_id", "address"}),
			Handler: func(state State, args map[string]any) (any, error) {
				id, _ := args["order_id"].(string)
				addr, _ := args["address"].(string)
				orders, _ := state["orders"].(map[string]any)
				rec, ok := orders[id].(map[string]any)
				if !ok {
					return map[string]any{"error": fmt.Sprintf("order %q not found", id)}, nil
				}
				status, _ := rec["status"].(string)
				if status != "pending" {
					return map[string]any{"error": fmt.Sprintf("order %q is in status %q and the shipping address is locked", id, status)}, nil
				}
				rec["shipping_address"] = addr
				return map[string]any{"order_id": id, "shipping_address": addr}, nil
			},
		},
		"get_product": {
			Name:        "get_product",
			Description: "Look up a product by its id.",
			InputSchema: objectSchema(map[string]any{
				"product_id": stringSchema("Product id, e.g. P-44"),
			}, []string{"product_id"}),
			Handler: func(state State, args map[string]any) (any, error) {
				id, _ := args["product_id"].(string)
				prods, _ := state["products"].(map[string]any)
				rec, ok := prods[id].(map[string]any)
				if !ok {
					return map[string]any{"error": fmt.Sprintf("product %q not found", id)}, nil
				}
				return rec, nil
			},
		},
		"search_products": {
			Name:        "search_products",
			Description: "Case-insensitive substring search across product names and descriptions. Returns matching product ids.",
			InputSchema: objectSchema(map[string]any{
				"query": stringSchema("Free-text query, e.g. \"red sneakers\""),
			}, []string{"query"}),
			Handler: func(state State, args map[string]any) (any, error) {
				q, _ := args["query"].(string)
				q = strings.ToLower(strings.TrimSpace(q))
				prods, _ := state["products"].(map[string]any)
				var hits []string
				for id, rec := range prods {
					m, ok := rec.(map[string]any)
					if !ok {
						continue
					}
					name, _ := m["name"].(string)
					desc, _ := m["description"].(string)
					if strings.Contains(strings.ToLower(name), q) || strings.Contains(strings.ToLower(desc), q) {
						hits = append(hits, id)
					}
				}
				return map[string]any{"product_ids": hits}, nil
			},
		},
	}
}

// NewRetailMiniDataset returns a small hand-curated retail task pack
// that exercises every retail tool at least once. We bundle this
// inline so a smoke run (CI / dev laptop) needs no external assets;
// the official τ-bench retail set (≈115 tasks) is downloaded
// separately and parsed via LoadDataset (forthcoming converter).
func NewRetailMiniDataset() *Dataset {
	baseState := func() State {
		return State{
			"customers": map[string]any{
				"CUST-1": map[string]any{"name": "Ada Lovelace", "email": "ada@example.com"},
				"CUST-2": map[string]any{"name": "Grace Hopper", "email": "grace@example.com"},
			},
			"products": map[string]any{
				"P-1": map[string]any{"name": "Red Sneakers", "description": "Bright red running shoes", "price": 79.99},
				"P-2": map[string]any{"name": "Blue Jacket", "description": "Lightweight blue running jacket", "price": 129.00},
				"P-3": map[string]any{"name": "Black Socks", "description": "Pack of 6 athletic socks in black", "price": 14.99},
			},
			"orders": map[string]any{
				"ORD-1001": map[string]any{
					"customer_id":      "CUST-1",
					"status":           "pending",
					"items":            []any{"P-1"},
					"shipping_address": "10 Computing Lane, London",
				},
				"ORD-1002": map[string]any{
					"customer_id":      "CUST-1",
					"status":           "processing",
					"items":            []any{"P-2", "P-3"},
					"shipping_address": "10 Computing Lane, London",
				},
				"ORD-1003": map[string]any{
					"customer_id":      "CUST-2",
					"status":           "delivered",
					"items":            []any{"P-3"},
					"shipping_address": "1 Naval Avenue, Washington",
				},
			},
		}
	}
	return &Dataset{
		Name: "retail-mini",
		Tasks: []Task{
			{
				ID:           "retail-cancel-pending",
				Domain:       "retail",
				Instruction:  "Hi, I'm customer CUST-1 and I'd like to cancel my order ORD-1001 because I ordered the wrong size. Please cancel it for me.",
				InitialState: baseState(),
				Expected: ExpectedOutcome{
					StateChecks: []StateCheck{
						{Path: "orders.ORD-1001.status", Equals: "cancelled"},
					},
					RequiredTools: []string{"cancel_order"},
				},
			},
			{
				ID:           "retail-cancel-processing",
				Domain:       "retail",
				Instruction:  "Hello, this is CUST-1. My order ORD-1002 is taking too long; cancel it please, reason: changed mind.",
				InitialState: baseState(),
				Expected: ExpectedOutcome{
					StateChecks: []StateCheck{
						{Path: "orders.ORD-1002.status", Equals: "cancelled"},
					},
					RequiredTools: []string{"cancel_order"},
				},
			},
			{
				ID:           "retail-update-shipping",
				Domain:       "retail",
				Instruction:  "Hi, I'm customer CUST-1. Please update the shipping address on order ORD-1001 to: 22 Engineering Way, Cambridge.",
				InitialState: baseState(),
				Expected: ExpectedOutcome{
					StateChecks: []StateCheck{
						{Path: "orders.ORD-1001.shipping_address", Equals: "22 Engineering Way, Cambridge"},
					},
					RequiredTools: []string{"update_shipping"},
				},
			},
			{
				ID:           "retail-cannot-cancel-delivered",
				Domain:       "retail",
				Instruction:  "This is CUST-2. Cancel my order ORD-1003 please.",
				InitialState: baseState(),
				Expected: ExpectedOutcome{
					// A correct agent recognises the order is already
					// delivered and does NOT mutate state. We assert
					// status stays "delivered".
					StateChecks: []StateCheck{
						{Path: "orders.ORD-1003.status", Equals: "delivered"},
					},
					// We deliberately do NOT require cancel_order to
					// be called: the model is free to detect the
					// situation from get_order alone and refuse
					// gracefully.
				},
			},
			{
				ID:           "retail-product-search",
				Domain:       "retail",
				Instruction:  "Hi, I'm looking for red sneakers. Can you find the product id and confirm the price for me?",
				InitialState: baseState(),
				Expected: ExpectedOutcome{
					// Information-only task: state should not change.
					// Success requires the agent to actually search.
					RequiredTools: []string{"search_products"},
				},
			},

			// ── Multi-turn dialog tasks ──────────────────────────────
			// The CustomerScenario field switches the harness from
			// single-shot to the customer-LLM dialog mode. The
			// scenario is private context the customer LLM acts out;
			// the agent only sees the LLM's natural-language replies.
			//
			// We deliberately make these scenarios test skills the
			// single-shot variants cannot:
			//   - identifier lookup ("I forgot my order id")
			//   - clarification handling (agent asks for missing info)
			//   - graceful refusal under multi-turn pressure
			{
				ID:               "retail-dialog-forgot-order-id",
				Domain:           "retail",
				CustomerScenario: "You are Ada Lovelace, customer id CUST-1. You want to cancel an order you placed because the size was wrong, but you cannot remember the order id. You DO remember it was your most recent pending order. Reason: ordered the wrong size. Do not paste this scenario verbatim — speak naturally.",
				CustomerOpening:  "Hi, I'd like to cancel one of my recent orders please. Ordered the wrong size.",
				InitialState:     baseState(),
				Expected: ExpectedOutcome{
					StateChecks: []StateCheck{
						{Path: "orders.ORD-1001.status", Equals: "cancelled"},
					},
					// We ALLOW the agent to either look the order up
					// by customer or just ask the customer. Either
					// path satisfies the goal, so RequiredTools only
					// pins the mutation.
					RequiredTools: []string{"cancel_order"},
				},
			},
			{
				ID:               "retail-dialog-refuse-delivered",
				Domain:           "retail",
				CustomerScenario: "You are Grace Hopper, customer id CUST-2. You want to cancel your order ORD-1003 because you changed your mind. The agent will likely tell you the order is already delivered and cannot be cancelled. When they do, accept the explanation politely and end the conversation.",
				CustomerOpening:  "Hi! I'd like to cancel my order ORD-1003 please. Changed my mind.",
				InitialState:     baseState(),
				Expected: ExpectedOutcome{
					// Correct agent refuses; state must not change.
					StateChecks: []StateCheck{
						{Path: "orders.ORD-1003.status", Equals: "delivered"},
					},
					// We do NOT require cancel_order; in fact a
					// well-calibrated agent should NOT call it.
				},
			},
		},
	}
}

// objectSchema is a tiny convenience for writing JSON-Schema fragments
// inline. The "additionalProperties: false" keeps the schema strict
// so a forgetful agent that adds bogus fields is forced to fix the
// call rather than silently being routed past validation.
func objectSchema(props map[string]any, required []string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
}

func stringSchema(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
