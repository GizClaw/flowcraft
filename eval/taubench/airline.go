package taubench

import (
	"fmt"
	"strings"
)

// NewAirlineTools returns the canonical airline-domain tool registry.
// Surface area mirrors the upstream τ-bench airline tools tightly
// enough that future upstream-JSON tasks (see the Roadmap in README)
// can be loaded without per-tool adapters.
//
// Tools:
//
//	get_user(user_id)                          → user record or {error}
//	get_reservation(reservation_id)            → reservation record or {error}
//	list_user_reservations(user_id)            → []reservation_id for user
//	cancel_reservation(reservation_id, reason) → status → "cancelled"; only "confirmed" / "pending" eligible
//	update_baggage(reservation_id, num_bags)   → sets reservations.<id>.baggage.checked
//	search_flight(origin, dest, date)          → []flight_id where route matches
//	get_flight(flight_id)                      → flight record or {error}
//
// "Status-protected" mutations match real airline policy: a flight
// that has already departed cannot be cancelled via the agent, and
// the handler returns an error payload (not a Go error) so the LLM
// can read the rejection and replan.
func NewAirlineTools() map[string]Tool {
	return map[string]Tool{
		"get_user": {
			Name:        "get_user",
			Description: "Look up a customer by their user id. Returns name, email, membership tier, payment methods.",
			InputSchema: objectSchema(map[string]any{
				"user_id": stringSchema("User id, e.g. USER-1"),
			}, []string{"user_id"}),
			Handler: func(state State, args map[string]any) (any, error) {
				id, _ := args["user_id"].(string)
				users, _ := state["users"].(map[string]any)
				rec, ok := users[id].(map[string]any)
				if !ok {
					return map[string]any{"error": fmt.Sprintf("user %q not found", id)}, nil
				}
				return rec, nil
			},
		},
		"get_reservation": {
			Name:        "get_reservation",
			Description: "Look up a reservation by id. Returns user_id, flights, passengers, baggage, status, cabin.",
			InputSchema: objectSchema(map[string]any{
				"reservation_id": stringSchema("Reservation id, e.g. RES-1"),
			}, []string{"reservation_id"}),
			Handler: func(state State, args map[string]any) (any, error) {
				id, _ := args["reservation_id"].(string)
				res, _ := state["reservations"].(map[string]any)
				rec, ok := res[id].(map[string]any)
				if !ok {
					return map[string]any{"error": fmt.Sprintf("reservation %q not found", id)}, nil
				}
				return rec, nil
			},
		},
		"list_user_reservations": {
			Name:        "list_user_reservations",
			Description: "List every reservation id belonging to a user.",
			InputSchema: objectSchema(map[string]any{
				"user_id": stringSchema("User id"),
			}, []string{"user_id"}),
			Handler: func(state State, args map[string]any) (any, error) {
				uid, _ := args["user_id"].(string)
				res, _ := state["reservations"].(map[string]any)
				var hits []string
				for id, rec := range res {
					if m, ok := rec.(map[string]any); ok && m["user_id"] == uid {
						hits = append(hits, id)
					}
				}
				return map[string]any{"reservation_ids": hits}, nil
			},
		},
		"cancel_reservation": {
			Name:        "cancel_reservation",
			Description: "Cancel a reservation. Only reservations in status \"confirmed\" or \"pending\" can be cancelled — \"departed\" / \"completed\" / \"already_cancelled\" return an error.",
			InputSchema: objectSchema(map[string]any{
				"reservation_id": stringSchema("Reservation id"),
				"reason":         stringSchema("Free-text reason"),
			}, []string{"reservation_id", "reason"}),
			Handler: func(state State, args map[string]any) (any, error) {
				id, _ := args["reservation_id"].(string)
				reason, _ := args["reason"].(string)
				res, _ := state["reservations"].(map[string]any)
				rec, ok := res[id].(map[string]any)
				if !ok {
					return map[string]any{"error": fmt.Sprintf("reservation %q not found", id)}, nil
				}
				status, _ := rec["status"].(string)
				if status != "confirmed" && status != "pending" {
					return map[string]any{"error": fmt.Sprintf("reservation %q is in status %q and cannot be cancelled", id, status)}, nil
				}
				rec["status"] = "cancelled"
				rec["cancellation_reason"] = reason
				return map[string]any{"reservation_id": id, "status": "cancelled"}, nil
			},
		},
		"update_baggage": {
			Name:        "update_baggage",
			Description: "Set the number of checked bags on a reservation. Only confirmed reservations can be modified.",
			InputSchema: objectSchema(map[string]any{
				"reservation_id": stringSchema("Reservation id"),
				"num_bags":       map[string]any{"type": "integer", "description": "New checked-bag count", "minimum": 0, "maximum": 4},
			}, []string{"reservation_id", "num_bags"}),
			Handler: func(state State, args map[string]any) (any, error) {
				id, _ := args["reservation_id"].(string)
				// JSON unmarshalling yields float64 for numeric
				// fields; we accept both representations because
				// some providers helpfully (and unhelpfully) string-
				// ify their arguments.
				n, ok := asInt(args["num_bags"])
				if !ok {
					return map[string]any{"error": "num_bags must be an integer"}, nil
				}
				if n < 0 || n > 4 {
					return map[string]any{"error": fmt.Sprintf("num_bags %d out of range [0,4]", n)}, nil
				}
				res, _ := state["reservations"].(map[string]any)
				rec, ok := res[id].(map[string]any)
				if !ok {
					return map[string]any{"error": fmt.Sprintf("reservation %q not found", id)}, nil
				}
				status, _ := rec["status"].(string)
				if status != "confirmed" {
					return map[string]any{"error": fmt.Sprintf("reservation %q is in status %q and baggage is locked", id, status)}, nil
				}
				baggage, _ := rec["baggage"].(map[string]any)
				if baggage == nil {
					baggage = map[string]any{}
					rec["baggage"] = baggage
				}
				baggage["checked"] = n
				return map[string]any{"reservation_id": id, "baggage": baggage}, nil
			},
		},
		"search_flight": {
			Name:        "search_flight",
			Description: "Find direct flights matching an origin / destination / date triple. Returns matching flight ids.",
			InputSchema: objectSchema(map[string]any{
				"origin":      stringSchema("3-letter airport code, e.g. JFK"),
				"destination": stringSchema("3-letter airport code, e.g. LAX"),
				"date":        stringSchema("YYYY-MM-DD"),
			}, []string{"origin", "destination", "date"}),
			Handler: func(state State, args map[string]any) (any, error) {
				origin, _ := args["origin"].(string)
				dest, _ := args["destination"].(string)
				date, _ := args["date"].(string)
				flights, _ := state["flights"].(map[string]any)
				var hits []string
				for id, rec := range flights {
					m, ok := rec.(map[string]any)
					if !ok {
						continue
					}
					if strings.EqualFold(m["origin"].(string), origin) &&
						strings.EqualFold(m["destination"].(string), dest) &&
						m["date"] == date {
						hits = append(hits, id)
					}
				}
				return map[string]any{"flight_ids": hits}, nil
			},
		},
		"get_flight": {
			Name:        "get_flight",
			Description: "Look up a flight by id. Returns origin/destination/date/price/aircraft.",
			InputSchema: objectSchema(map[string]any{
				"flight_id": stringSchema("Flight id, e.g. FL-100"),
			}, []string{"flight_id"}),
			Handler: func(state State, args map[string]any) (any, error) {
				id, _ := args["flight_id"].(string)
				flights, _ := state["flights"].(map[string]any)
				rec, ok := flights[id].(map[string]any)
				if !ok {
					return map[string]any{"error": fmt.Sprintf("flight %q not found", id)}, nil
				}
				return rec, nil
			},
		},
	}
}

// asInt accepts either a float64 (the default JSON number type after
// json.Unmarshal into map[string]any) or a numeric string. Returns
// (n, true) on success, (0, false) on anything else.
func asInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	case int64:
		return int(x), true
	case string:
		var n int
		_, err := fmt.Sscanf(x, "%d", &n)
		return n, err == nil
	}
	return 0, false
}

// NewAirlineMiniDataset bundles 5 hand-curated airline tasks
// exercising each tool at least once: cancel (status-eligible),
// cancel (status-protected refusal), update-baggage, search-info, and
// a multi-turn dialog where the customer doesn't remember their
// reservation id.
func NewAirlineMiniDataset() *Dataset {
	baseState := func() State {
		return State{
			"users": map[string]any{
				"USER-1": map[string]any{"name": "Ada Lovelace", "email": "ada@example.com", "tier": "gold"},
				"USER-2": map[string]any{"name": "Grace Hopper", "email": "grace@example.com", "tier": "silver"},
			},
			"flights": map[string]any{
				"FL-100": map[string]any{"origin": "JFK", "destination": "LAX", "date": "2026-06-01", "price": 320.0},
				"FL-101": map[string]any{"origin": "JFK", "destination": "LAX", "date": "2026-06-01", "price": 420.0},
				"FL-200": map[string]any{"origin": "SFO", "destination": "JFK", "date": "2026-06-02", "price": 380.0},
			},
			"reservations": map[string]any{
				"RES-1": map[string]any{
					"user_id":    "USER-1",
					"flight_id":  "FL-100",
					"status":     "confirmed",
					"passengers": []any{"Ada Lovelace"},
					"baggage":    map[string]any{"checked": 1},
				},
				"RES-2": map[string]any{
					"user_id":    "USER-1",
					"flight_id":  "FL-200",
					"status":     "confirmed",
					"passengers": []any{"Ada Lovelace"},
					"baggage":    map[string]any{"checked": 0},
				},
				"RES-3": map[string]any{
					"user_id":    "USER-2",
					"flight_id":  "FL-101",
					"status":     "departed",
					"passengers": []any{"Grace Hopper"},
					"baggage":    map[string]any{"checked": 2},
				},
			},
		}
	}
	return &Dataset{
		Name: "airline-mini",
		Tasks: []Task{
			{
				ID:           "airline-cancel-reservation",
				Domain:       "airline",
				Instruction:  "Hi, I'm USER-1. Please cancel my reservation RES-1 — my plans changed.",
				InitialState: baseState(),
				Expected: ExpectedOutcome{
					StateChecks: []StateCheck{
						{Path: "reservations.RES-1.status", Equals: "cancelled"},
					},
					RequiredTools: []string{"cancel_reservation"},
				},
			},
			{
				ID:           "airline-add-baggage",
				Domain:       "airline",
				Instruction:  "I'm USER-1. Please add 2 checked bags to reservation RES-2 (so the total is 2).",
				InitialState: baseState(),
				Expected: ExpectedOutcome{
					StateChecks: []StateCheck{
						{Path: "reservations.RES-2.baggage.checked", Equals: 2},
					},
					RequiredTools: []string{"update_baggage"},
				},
			},
			{
				ID:           "airline-search-info",
				Domain:       "airline",
				Instruction:  "Hi, can you check what direct flights are available from JFK to LAX on 2026-06-01? I just need the options, not a booking.",
				InitialState: baseState(),
				Expected: ExpectedOutcome{
					RequiredTools: []string{"search_flight"},
				},
			},
			{
				ID:           "airline-refuse-departed",
				Domain:       "airline",
				Instruction:  "This is USER-2. Cancel my reservation RES-3 please.",
				InitialState: baseState(),
				Expected: ExpectedOutcome{
					// Departed flights cannot be cancelled. A correct
					// agent recognises this and leaves the status
					// untouched. We do NOT require any specific tool
					// because the model may diagnose via get_reservation
					// alone before refusing.
					StateChecks: []StateCheck{
						{Path: "reservations.RES-3.status", Equals: "departed"},
					},
				},
			},
			{
				ID:               "airline-dialog-cancel-via-lookup",
				Domain:           "airline",
				CustomerScenario: "You are Ada Lovelace, user id USER-1. You want to cancel your upcoming flight from JFK to LAX on 2026-06-01. You do NOT remember the reservation id; the agent will need to look it up from your user id. Reason: plans changed. Stay polite; do NOT paste the scenario text.",
				CustomerOpening:  "Hi, I need to cancel my upcoming flight from JFK to LAX. Plans changed.",
				InitialState:     baseState(),
				Expected: ExpectedOutcome{
					StateChecks: []StateCheck{
						{Path: "reservations.RES-1.status", Equals: "cancelled"},
					},
					RequiredTools: []string{"cancel_reservation"},
				},
			},
		},
	}
}
