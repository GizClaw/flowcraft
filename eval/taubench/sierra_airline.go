package taubench

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// NewSierraAirlineTools returns the canonical 14-tool airline-domain
// registry ported from sierra-research/tau-bench
// tau_bench/envs/airline/tools/ at commit 59a200c6 (2026-03-18).
//
// Airline state shape (top-level keys): "flights", "reservations",
// "users". The contract differs from retail in two important
// places worth knowing if you maintain both:
//
//   - Payment-method records use `amount` instead of `balance`.
//     (Yes, this is Sierra's naming, not ours; preserved verbatim
//     so the gold ExpectedFinalState comparison stays tight.)
//   - `payment_history` entries carry `payment_id` + `amount` only;
//     no `transaction_type`. Cancellation records refunds as
//     entries with negative amounts.
func NewSierraAirlineTools() map[string]Tool {
	return map[string]Tool{
		// --- read tools ---
		"get_reservation_details": {
			Name:        "get_reservation_details",
			Description: "Get the details of a reservation.",
			InputSchema: schemaObject(map[string]any{
				"reservation_id": schemaString("The reservation id, such as '8JX2WO'."),
			}, []string{"reservation_id"}),
			Handler: airGetReservationDetails,
		},
		"get_user_details": {
			Name:        "get_user_details",
			Description: "Get the details of an user, including their reservations.",
			InputSchema: schemaObject(map[string]any{
				"user_id": schemaString("The user id, such as 'sara_doe_496'."),
			}, []string{"user_id"}),
			Handler: airGetUserDetails,
		},
		"list_all_airports": {
			Name:        "list_all_airports",
			Description: "List all airports and their cities.",
			InputSchema: schemaObject(nil, nil),
			Handler:     airListAllAirports,
		},
		"search_direct_flight": {
			Name:        "search_direct_flight",
			Description: "Search direct flights between two cities on a specific date.",
			InputSchema: schemaObject(map[string]any{
				"origin":      schemaString("The origin city airport in three letters, such as 'JFK'."),
				"destination": schemaString("The destination city airport in three letters, such as 'LAX'."),
				"date":        schemaString("The date of the flight in the format 'YYYY-MM-DD', such as '2024-01-01'."),
			}, []string{"origin", "destination", "date"}),
			Handler: airSearchDirectFlight,
		},
		"search_onestop_flight": {
			Name:        "search_onestop_flight",
			Description: "Search direct flights between two cities on a specific date.",
			InputSchema: schemaObject(map[string]any{
				"origin":      schemaString("The origin city airport in three letters, such as 'JFK'."),
				"destination": schemaString("The destination city airport in three letters, such as 'LAX'."),
				"date":        schemaString("The date of the flight in the format 'YYYY-MM-DD', such as '2024-05-01'."),
			}, []string{"origin", "destination", "date"}),
			Handler: airSearchOnestopFlight,
		},
		// --- mutating tools ---
		"book_reservation": {
			Name:        "book_reservation",
			Description: "Book a reservation.",
			InputSchema: airBookReservationSchema(),
			Handler:     airBookReservation,
		},
		"cancel_reservation": {
			Name:        "cancel_reservation",
			Description: "Cancel the whole reservation.",
			InputSchema: schemaObject(map[string]any{
				"reservation_id": schemaString("The reservation ID, such as 'ZFA04Y'."),
			}, []string{"reservation_id"}),
			Handler: airCancelReservation,
		},
		"send_certificate": {
			Name:        "send_certificate",
			Description: "Send a certificate to a user. Be careful!",
			InputSchema: schemaObject(map[string]any{
				"user_id": schemaString("The ID of the user to book the reservation, such as 'sara_doe_496'."),
				"amount":  map[string]any{"type": "number", "description": "Certificate amount to send."},
			}, []string{"user_id", "amount"}),
			Handler: airSendCertificate,
		},
		"update_reservation_baggages": {
			Name:        "update_reservation_baggages",
			Description: "Update the baggage information of a reservation.",
			InputSchema: schemaObject(map[string]any{
				"reservation_id":   schemaString("The reservation ID, such as 'ZFA04Y'."),
				"total_baggages":   map[string]any{"type": "integer", "description": "The updated total number of baggage items included in the reservation."},
				"nonfree_baggages": map[string]any{"type": "integer", "description": "The updated number of non-free baggage items included in the reservation."},
				"payment_id":       schemaString("The payment id stored in user profile, such as 'credit_card_7815826', 'gift_card_7815826', 'certificate_7815826'."),
			}, []string{"reservation_id", "total_baggages", "nonfree_baggages", "payment_id"}),
			Handler: airUpdateReservationBaggages,
		},
		"update_reservation_flights": {
			Name:        "update_reservation_flights",
			Description: "Update the flight information of a reservation.",
			InputSchema: airUpdateReservationFlightsSchema(),
			Handler:     airUpdateReservationFlights,
		},
		"update_reservation_passengers": {
			Name:        "update_reservation_passengers",
			Description: "Update the passenger information of a reservation.",
			InputSchema: airUpdateReservationPassengersSchema(),
			Handler:     airUpdateReservationPassengers,
		},
		// --- aux tools ---
		"calculate": {
			Name:        "calculate",
			Description: "Calculate the result of a mathematical expression.",
			InputSchema: schemaObject(map[string]any{
				"expression": schemaString("The mathematical expression to calculate, such as '2 + 2'. The expression can contain numbers, operators (+, -, *, /), parentheses, and spaces."),
			}, []string{"expression"}),
			Handler: sierraCalculate,
		},
		"think": {
			Name:        "think",
			Description: "Use the tool to think about something. It will not obtain new information or change the database, but just append the thought to the log. Use it when complex reasoning or some cache memory is needed.",
			InputSchema: schemaObject(map[string]any{
				"thought": schemaString("A thought to think about."),
			}, []string{"thought"}),
			Handler: sierraThink,
		},
		"transfer_to_human_agents": {
			Name:        "transfer_to_human_agents",
			Description: "Transfer the user to a human agent, with a summary of the user's issue. Only transfer if the user explicitly asks for a human agent, or if the user's issue cannot be resolved by the agent with the available tools.",
			InputSchema: schemaObject(map[string]any{
				"summary": schemaString("A summary of the user's issue."),
			}, []string{"summary"}),
			Handler: sierraTransferToHumanRetail,
		},
	}
}

// --- read handlers ---

func airGetReservationDetails(state State, args map[string]any) (any, error) {
	rid, err := argString(args, "reservation_id")
	if err != nil {
		return nil, err
	}
	res := asMap(state["reservations"])
	r, ok := res[rid]
	if !ok {
		return "Error: user not found", nil // verbatim from Sierra (yes, "user")
	}
	return jsonString(r)
}

func airGetUserDetails(state State, args map[string]any) (any, error) {
	uid, err := argString(args, "user_id")
	if err != nil {
		return nil, err
	}
	users := asMap(state["users"])
	u, ok := users[uid]
	if !ok {
		return "Error: user not found", nil
	}
	return jsonString(u)
}

func airListAllAirports(_ State, _ map[string]any) (any, error) {
	airports := []string{"SFO", "JFK", "LAX", "ORD", "DFW", "DEN", "SEA", "ATL", "MIA", "BOS",
		"PHX", "IAH", "LAS", "MCO", "EWR", "CLT", "MSP", "DTW", "PHL", "LGA"}
	cities := []string{"San Francisco", "New York", "Los Angeles", "Chicago", "Dallas",
		"Denver", "Seattle", "Atlanta", "Miami", "Boston", "Phoenix", "Houston", "Las Vegas",
		"Orlando", "Newark", "Charlotte", "Minneapolis", "Detroit", "Philadelphia", "LaGuardia"}
	out := make(map[string]string, len(airports))
	for i, a := range airports {
		out[a] = cities[i]
	}
	return jsonString(out)
}

func airSearchDirectFlight(state State, args map[string]any) (any, error) {
	origin, err := argString(args, "origin")
	if err != nil {
		return nil, err
	}
	destination, err := argString(args, "destination")
	if err != nil {
		return nil, err
	}
	date, err := argString(args, "date")
	if err != nil {
		return nil, err
	}
	flights := asMap(state["flights"])
	keys := sortedKeys(flights)
	results := make([]any, 0)
	for _, k := range keys {
		flight := asMap(flights[k])
		if flight["origin"] != origin || flight["destination"] != destination {
			continue
		}
		dates := asMap(flight["dates"])
		dateData := asMap(dates[date])
		if dateData == nil || dateData["status"] != "available" {
			continue
		}
		entry := copyMapExcept(flight, "dates")
		for ek, ev := range dateData {
			entry[ek] = ev
		}
		results = append(results, entry)
	}
	return jsonString(results)
}

func airSearchOnestopFlight(state State, args map[string]any) (any, error) {
	origin, err := argString(args, "origin")
	if err != nil {
		return nil, err
	}
	destination, err := argString(args, "destination")
	if err != nil {
		return nil, err
	}
	date, err := argString(args, "date")
	if err != nil {
		return nil, err
	}
	flights := asMap(state["flights"])
	keys := sortedKeys(flights)
	results := make([]any, 0)
	for _, k1 := range keys {
		f1 := asMap(flights[k1])
		if f1["origin"] != origin {
			continue
		}
		for _, k2 := range keys {
			f2 := asMap(flights[k2])
			if f2["destination"] != destination {
				continue
			}
			if f1["destination"] != f2["origin"] {
				continue
			}
			arrivalStr, _ := f1["scheduled_arrival_time_est"].(string)
			deptStr, _ := f2["scheduled_departure_time_est"].(string)
			date2 := date
			if strings.Contains(arrivalStr, "+1") {
				lastTwo := date[len(date)-2:]
				dd, err := strconv.Atoi(lastTwo)
				if err == nil {
					date2 = fmt.Sprintf("2024-05-%d", dd+1)
				}
			}
			if arrivalStr > deptStr {
				continue
			}
			d1 := asMap(asMap(f1["dates"])[date])
			d2 := asMap(asMap(f2["dates"])[date2])
			if d1 == nil || d2 == nil {
				continue
			}
			if d1["status"] != "available" || d2["status"] != "available" {
				continue
			}
			r1 := copyMapExcept(f1, "dates")
			for k, v := range d1 {
				r1[k] = v
			}
			r1["date"] = date
			r2 := copyMapExcept(f2, "dates")
			for k, v := range d2 {
				r2[k] = v
			}
			r2["date"] = date2
			results = append(results, []any{r1, r2})
		}
	}
	return jsonString(results)
}

// --- mutating handlers ---

func airBookReservation(state State, args map[string]any) (any, error) {
	userID, err := argString(args, "user_id")
	if err != nil {
		return nil, err
	}
	origin, _ := argString(args, "origin")
	destination, _ := argString(args, "destination")
	flightType, _ := argString(args, "flight_type")
	cabin, _ := argString(args, "cabin")
	insurance, _ := argString(args, "insurance")
	flightsArg := asSlice(args["flights"])
	passengersArg := asSlice(args["passengers"])
	paymentsArg := asSlice(args["payment_methods"])
	totalBaggages := int(asFloat(args["total_baggages"]))
	nonfreeBaggages := int(asFloat(args["nonfree_baggages"]))

	reservations := asMap(state["reservations"])
	users := asMap(state["users"])
	user, ok := users[userID]
	if !ok {
		return "Error: user not found", nil
	}
	userMap := asMap(user)

	// Sierra: assume each task makes at most 3 reservations.
	rid := "HATHAT"
	if _, exists := reservations[rid]; exists {
		rid = "HATHAU"
		if _, exists2 := reservations[rid]; exists2 {
			rid = "HATHAV"
		}
	}

	// deepcopy the flights array so subsequent mutations don't
	// alias the caller-provided slice.
	flightsCopy := make([]any, len(flightsArg))
	for i, f := range flightsArg {
		flightsCopy[i] = deepCopyAny(f)
	}

	reservation := map[string]any{
		"reservation_id":   rid,
		"user_id":          userID,
		"origin":           origin,
		"destination":      destination,
		"flight_type":      flightType,
		"cabin":            cabin,
		"flights":          flightsCopy,
		"passengers":       passengersArg,
		"payment_history":  paymentsArg,
		"created_at":       "2024-05-15T15:00:00",
		"total_baggages":   totalBaggages,
		"nonfree_baggages": nonfreeBaggages,
		"insurance":        insurance,
	}

	flightsDB := asMap(state["flights"])
	totalPrice := 0.0
	for _, fAny := range flightsCopy {
		flight := asMap(fAny)
		flightNumber, _ := flight["flight_number"].(string)
		flightDate, _ := flight["date"].(string)
		flightDataAny, ok := flightsDB[flightNumber]
		if !ok {
			return fmt.Sprintf("Error: flight %s not found", flightNumber), nil
		}
		flightData := asMap(flightDataAny)
		dates := asMap(flightData["dates"])
		dateData, ok := dates[flightDate]
		if !ok {
			return fmt.Sprintf("Error: flight %s not found on date %s", flightNumber, flightDate), nil
		}
		flightDateData := asMap(dateData)
		if flightDateData["status"] != "available" {
			return fmt.Sprintf("Error: flight %s not available on date %s", flightNumber, flightDate), nil
		}
		availableSeats := asMap(flightDateData["available_seats"])
		if int(asFloat(availableSeats[cabin])) < len(passengersArg) {
			return fmt.Sprintf("Error: not enough seats on flight %s", flightNumber), nil
		}
		prices := asMap(flightDateData["prices"])
		price := asFloat(prices[cabin])
		flight["price"] = price
		flight["origin"] = flightData["origin"]
		flight["destination"] = flightData["destination"]
		totalPrice += price * float64(len(passengersArg))
	}

	if insurance == "yes" {
		totalPrice += 30 * float64(len(passengersArg))
	}
	totalPrice += 50 * float64(nonfreeBaggages)

	pms := asMap(userMap["payment_methods"])
	for _, pAny := range paymentsArg {
		payment := asMap(pAny)
		paymentID, _ := payment["payment_id"].(string)
		amt := asFloat(payment["amount"])
		pm, ok := pms[paymentID]
		if !ok {
			return fmt.Sprintf("Error: payment method %s not found", paymentID), nil
		}
		pmMap := asMap(pm)
		src, _ := pmMap["source"].(string)
		if src == "gift_card" || src == "certificate" {
			if asFloat(pmMap["amount"]) < amt {
				return fmt.Sprintf("Error: not enough balance in payment method %s", paymentID), nil
			}
		}
	}
	var paidTotal float64
	for _, pAny := range paymentsArg {
		paidTotal += asFloat(asMap(pAny)["amount"])
	}
	if paidTotal != totalPrice {
		return fmt.Sprintf("Error: payment amount does not add up, total price is %s, but paid %s",
			formatNumber(totalPrice), formatNumber(paidTotal)), nil
	}

	for _, pAny := range paymentsArg {
		payment := asMap(pAny)
		paymentID, _ := payment["payment_id"].(string)
		amt := asFloat(payment["amount"])
		pm := asMap(pms[paymentID])
		src, _ := pm["source"].(string)
		switch src {
		case "gift_card":
			pm["amount"] = asFloat(pm["amount"]) - amt
		case "certificate":
			delete(pms, paymentID)
		}
	}

	reservations[rid] = reservation
	userReservations := asSlice(userMap["reservations"])
	userReservations = append(userReservations, rid)
	userMap["reservations"] = userReservations
	return jsonString(reservation)
}

func airCancelReservation(state State, args map[string]any) (any, error) {
	rid, err := argString(args, "reservation_id")
	if err != nil {
		return nil, err
	}
	reservations := asMap(state["reservations"])
	r, ok := reservations[rid]
	if !ok {
		return "Error: reservation not found", nil
	}
	res := asMap(r)
	history := asSlice(res["payment_history"])
	refunds := make([]any, 0, len(history))
	for _, pAny := range history {
		p := asMap(pAny)
		refunds = append(refunds, map[string]any{
			"payment_id": p["payment_id"],
			"amount":     -asFloat(p["amount"]),
		})
	}
	res["payment_history"] = append(history, refunds...)
	res["status"] = "cancelled"
	return jsonString(res)
}

func airSendCertificate(state State, args map[string]any) (any, error) {
	userID, err := argString(args, "user_id")
	if err != nil {
		return nil, err
	}
	amount := asFloat(args["amount"])
	users := asMap(state["users"])
	u, ok := users[userID]
	if !ok {
		return "Error: user not found", nil
	}
	user := asMap(u)
	pms := asMap(user["payment_methods"])
	for _, id := range []string{"certificate_3221322", "certificate_3221323", "certificate_3221324"} {
		if _, exists := pms[id]; exists {
			continue
		}
		pms[id] = map[string]any{
			"source": "certificate",
			"amount": amount,
			"id":     id,
		}
		return fmt.Sprintf("Certificate %s added to user %s with amount %s.", id, userID, formatNumber(amount)), nil
	}
	// Sierra's Python returns None implicitly when all 3 slots are taken;
	// we surface a plain string so the harness doesn't see a nil.
	return "", nil
}

func airUpdateReservationBaggages(state State, args map[string]any) (any, error) {
	rid, err := argString(args, "reservation_id")
	if err != nil {
		return nil, err
	}
	totalBaggages := int(asFloat(args["total_baggages"]))
	nonfreeBaggages := int(asFloat(args["nonfree_baggages"]))
	paymentID, err := argString(args, "payment_id")
	if err != nil {
		return nil, err
	}
	users := asMap(state["users"])
	reservations := asMap(state["reservations"])
	r, ok := reservations[rid]
	if !ok {
		return "Error: reservation not found", nil
	}
	res := asMap(r)
	delta := nonfreeBaggages - int(asFloat(res["nonfree_baggages"]))
	if delta < 0 {
		delta = 0
	}
	totalPrice := float64(50 * delta)

	userID, _ := res["user_id"].(string)
	pms := asMap(asMap(users[userID])["payment_methods"])
	pm, ok := pms[paymentID]
	if !ok {
		return "Error: payment method not found", nil
	}
	pmMap := asMap(pm)
	src, _ := pmMap["source"].(string)
	if src == "certificate" {
		return "Error: certificate cannot be used to update reservation", nil
	}
	if src == "gift_card" && asFloat(pmMap["amount"]) < totalPrice {
		return "Error: gift card balance is not enough", nil
	}

	res["total_baggages"] = totalBaggages
	res["nonfree_baggages"] = nonfreeBaggages
	if src == "gift_card" {
		pmMap["amount"] = asFloat(pmMap["amount"]) - totalPrice
	}
	if totalPrice != 0 {
		history := asSlice(res["payment_history"])
		history = append(history, map[string]any{
			"payment_id": paymentID,
			"amount":     totalPrice,
		})
		res["payment_history"] = history
	}
	return jsonString(res)
}

func airUpdateReservationFlights(state State, args map[string]any) (any, error) {
	rid, err := argString(args, "reservation_id")
	if err != nil {
		return nil, err
	}
	cabin, err := argString(args, "cabin")
	if err != nil {
		return nil, err
	}
	paymentID, err := argString(args, "payment_id")
	if err != nil {
		return nil, err
	}
	flightsArg := asSlice(args["flights"])
	users := asMap(state["users"])
	reservations := asMap(state["reservations"])
	r, ok := reservations[rid]
	if !ok {
		return "Error: reservation not found", nil
	}
	res := asMap(r)

	// deepcopy
	flightsCopy := make([]any, len(flightsArg))
	for i, f := range flightsArg {
		flightsCopy[i] = deepCopyAny(f)
	}

	existingFlights := asSlice(res["flights"])
	flightsDB := asMap(state["flights"])
	totalPrice := 0.0
	resCabin, _ := res["cabin"].(string)
	passengerN := len(asSlice(res["passengers"]))
	for _, fAny := range flightsCopy {
		flight := asMap(fAny)
		fn, _ := flight["flight_number"].(string)
		fd, _ := flight["date"].(string)
		// existing flight pass-through
		var existing map[string]any
		for _, ex := range existingFlights {
			exMap := asMap(ex)
			if exMap["flight_number"] == fn && exMap["date"] == fd && cabin == resCabin {
				existing = exMap
				break
			}
		}
		if existing != nil {
			price := asFloat(existing["price"])
			totalPrice += price * float64(passengerN)
			flight["price"] = price
			flight["origin"] = existing["origin"]
			flight["destination"] = existing["destination"]
			continue
		}
		flightDataAny, ok := flightsDB[fn]
		if !ok {
			return fmt.Sprintf("Error: flight %s not found", fn), nil
		}
		flightData := asMap(flightDataAny)
		dates := asMap(flightData["dates"])
		dateData, ok := dates[fd]
		if !ok {
			return fmt.Sprintf("Error: flight %s not found on date %s", fn, fd), nil
		}
		fdd := asMap(dateData)
		if fdd["status"] != "available" {
			return fmt.Sprintf("Error: flight %s not available on date %s", fn, fd), nil
		}
		seats := asMap(fdd["available_seats"])
		if int(asFloat(seats[cabin])) < passengerN {
			return fmt.Sprintf("Error: not enough seats on flight %s", fn), nil
		}
		prices := asMap(fdd["prices"])
		price := asFloat(prices[cabin])
		flight["price"] = price
		flight["origin"] = flightData["origin"]
		flight["destination"] = flightData["destination"]
		totalPrice += price * float64(passengerN)
	}
	var prevTotal float64
	for _, ex := range existingFlights {
		prevTotal += asFloat(asMap(ex)["price"])
	}
	totalPrice -= prevTotal * float64(passengerN)

	userID, _ := res["user_id"].(string)
	pms := asMap(asMap(users[userID])["payment_methods"])
	pm, ok := pms[paymentID]
	if !ok {
		return "Error: payment method not found", nil
	}
	pmMap := asMap(pm)
	src, _ := pmMap["source"].(string)
	if src == "certificate" {
		return "Error: certificate cannot be used to update reservation", nil
	}
	if src == "gift_card" && asFloat(pmMap["amount"]) < totalPrice {
		return "Error: gift card balance is not enough", nil
	}
	if src == "gift_card" {
		pmMap["amount"] = asFloat(pmMap["amount"]) - totalPrice
	}
	res["flights"] = flightsCopy
	if totalPrice != 0 {
		history := asSlice(res["payment_history"])
		history = append(history, map[string]any{
			"payment_id": paymentID,
			"amount":     totalPrice,
		})
		res["payment_history"] = history
	}
	return jsonString(res)
}

func airUpdateReservationPassengers(state State, args map[string]any) (any, error) {
	rid, err := argString(args, "reservation_id")
	if err != nil {
		return nil, err
	}
	passengersArg := asSlice(args["passengers"])
	reservations := asMap(state["reservations"])
	r, ok := reservations[rid]
	if !ok {
		return "Error: reservation not found", nil
	}
	res := asMap(r)
	if len(passengersArg) != len(asSlice(res["passengers"])) {
		return "Error: number of passengers does not match", nil
	}
	res["passengers"] = passengersArg
	return jsonString(res)
}

// --- schema helpers for the more complex airline tools ---

func airBookReservationSchema() map[string]any {
	return schemaObject(map[string]any{
		"user_id":     schemaString("The ID of the user to book the reservation, such as 'sara_doe_496'."),
		"origin":      schemaString("The IATA code for the origin city, such as 'SFO'."),
		"destination": schemaString("The IATA code for the destination city, such as 'JFK'."),
		"flight_type": map[string]any{"type": "string", "enum": []any{"one_way", "round_trip"}},
		"cabin":       map[string]any{"type": "string", "enum": []any{"basic_economy", "economy", "business"}},
		"flights": map[string]any{
			"type":        "array",
			"description": "An array of objects containing details about each piece of flight.",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"flight_number": schemaString("Flight number, such as 'HAT001'."),
					"date":          schemaString("The date for the flight in the format 'YYYY-MM-DD', such as '2024-05-01'."),
				},
				"required": []any{"flight_number", "date"},
			},
		},
		"passengers":       airPassengersSchema("An array of objects containing details about each passenger."),
		"payment_methods":  airPaymentMethodsSchema(),
		"total_baggages":   map[string]any{"type": "integer", "description": "The total number of baggage items included in the reservation."},
		"nonfree_baggages": map[string]any{"type": "integer", "description": "The number of non-free baggage items included in the reservation."},
		"insurance":        map[string]any{"type": "string", "enum": []any{"yes", "no"}},
	}, []string{"user_id", "origin", "destination", "flight_type", "cabin", "flights", "passengers", "payment_methods", "total_baggages", "nonfree_baggages", "insurance"})
}

func airUpdateReservationFlightsSchema() map[string]any {
	return schemaObject(map[string]any{
		"reservation_id": schemaString("The reservation ID, such as 'ZFA04Y'."),
		"cabin":          map[string]any{"type": "string", "enum": []any{"basic_economy", "economy", "business"}},
		"flights": map[string]any{
			"type":        "array",
			"description": "An array of objects containing details about each piece of flight in the ENTIRE new reservation. Even if the a flight segment is not changed, it should still be included in the array.",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"flight_number": schemaString("Flight number, such as 'HAT001'."),
					"date":          schemaString("The date for the flight in the format 'YYYY-MM-DD', such as '2024-05-01'."),
				},
				"required": []any{"flight_number", "date"},
			},
		},
		"payment_id": schemaString("The payment id stored in user profile, such as 'credit_card_7815826', 'gift_card_7815826', 'certificate_7815826'."),
	}, []string{"reservation_id", "cabin", "flights", "payment_id"})
}

func airUpdateReservationPassengersSchema() map[string]any {
	return schemaObject(map[string]any{
		"reservation_id": schemaString("The reservation ID, such as 'ZFA04Y'."),
		"passengers":     airPassengersSchema("An array of objects containing details about each passenger."),
	}, []string{"reservation_id", "passengers"})
}

func airPassengersSchema(desc string) map[string]any {
	return map[string]any{
		"type":        "array",
		"description": desc,
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"first_name": schemaString("The first name of the passenger, such as 'Noah'."),
				"last_name":  schemaString("The last name of the passenger, such as 'Brown'."),
				"dob":        schemaString("The date of birth of the passenger in the format 'YYYY-MM-DD', such as '1990-01-01'."),
			},
			"required": []any{"first_name", "last_name", "dob"},
		},
	}
}

func airPaymentMethodsSchema() map[string]any {
	return map[string]any{
		"type":        "array",
		"description": "An array of objects containing details about each payment method.",
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"payment_id": schemaString("The payment id stored in user profile, such as 'credit_card_7815826', 'gift_card_7815826', 'certificate_7815826'."),
				"amount":     map[string]any{"type": "number", "description": "The amount to be paid."},
			},
			"required": []any{"payment_id", "amount"},
		},
	}
}

// --- tiny helpers (airline-private) ---

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func copyMapExcept(m map[string]any, skip string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k == skip {
			continue
		}
		out[k] = v
	}
	return out
}

func deepCopyAny(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			out[k] = deepCopyAny(vv)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			out[i] = deepCopyAny(vv)
		}
		return out
	}
	return v
}

func formatNumber(v float64) string {
	// Sierra interpolates Python's number stringification; for an
	// integer-valued float that's "123.0" -> "123" via `int(...)`
	// in some places and full repr in others. The error message
	// path uses Python's default repr which is "%g"-equivalent.
	return strconv.FormatFloat(v, 'g', -1, 64)
}
