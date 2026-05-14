package taubench

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// NewSierraRetailTools returns the canonical 16-tool retail-domain
// registry ported from sierra-research/tau-bench
// tau_bench/envs/retail/tools/ at commit 59a200c6 (2026-03-18).
// Used in conjunction with the staged initial_state.json + tasks_test.json
// produced by eval/taubench/sierra/prep.py; the shadow runner in
// upstream.go executes each task's gold action sequence against a
// clone of the initial state and snapshots the resulting State as
// the ExpectedFinalState.
//
// Differences from NewRetailTools (the hand-curated mini pack):
//
//   - Tool names, kwargs, and state shapes follow Sierra verbatim
//     (e.g. "cancel_pending_order" requires `reason ∈ {"no longer
//     needed", "ordered by mistake"}`, our mini "cancel_order"
//     accepts any reason). Mini and Sierra tool registries must
//     not be merged — they read incompatible state shapes.
//   - State is read as nested maps keyed by string ids; every
//     mutation is in-place on the supplied State map so shadowRun
//     captures the final state by snapshotting the same reference.
//   - Error returns are *strings*, not Go errors. Sierra's contract
//     is that a tool returning "Error: ..." represents a domain-rule
//     violation the agent should reason about; only a Go-level
//     panic (malformed kwargs, missing state subtree) is surfaced
//     as an error from Handler.
func NewSierraRetailTools() map[string]Tool {
	return map[string]Tool{
		// --- read tools ---
		"get_order_details": {
			Name:        "get_order_details",
			Description: "Get the status and details of an order.",
			InputSchema: schemaObject(map[string]any{
				"order_id": schemaString("The order id, such as '#W0000000'. Be careful there is a '#' symbol at the beginning of the order id."),
			}, []string{"order_id"}),
			Handler: sierraGetOrderDetails,
		},
		"get_product_details": {
			Name:        "get_product_details",
			Description: "Get the inventory details of a product.",
			InputSchema: schemaObject(map[string]any{
				"product_id": schemaString("The product id, such as '6086499569'. Be careful the product id is different from the item id."),
			}, []string{"product_id"}),
			Handler: sierraGetProductDetails,
		},
		"get_user_details": {
			Name:        "get_user_details",
			Description: "Get the details of a user, including their orders.",
			InputSchema: schemaObject(map[string]any{
				"user_id": schemaString("The user id, such as 'sara_doe_496'."),
			}, []string{"user_id"}),
			Handler: sierraGetUserDetails,
		},
		"find_user_id_by_email": {
			Name:        "find_user_id_by_email",
			Description: "Find user id by email. If the user is not found, the function will return an error message.",
			InputSchema: schemaObject(map[string]any{
				"email": schemaString("The email of the user, such as 'something@example.com'."),
			}, []string{"email"}),
			Handler: sierraFindUserByEmail,
		},
		"find_user_id_by_name_zip": {
			Name: "find_user_id_by_name_zip",
			Description: "Find user id by first name, last name, and zip code. If the user is not found, the function " +
				"will return an error message. By default, find user id by email, and only call this function if the " +
				"user is not found by email or cannot remember email.",
			InputSchema: schemaObject(map[string]any{
				"first_name": schemaString("The first name of the customer, such as 'John'."),
				"last_name":  schemaString("The last name of the customer, such as 'Doe'."),
				"zip":        schemaString("The zip code of the customer, such as '12345'."),
			}, []string{"first_name", "last_name", "zip"}),
			Handler: sierraFindUserByNameZip,
		},
		"list_all_product_types": {
			Name:        "list_all_product_types",
			Description: "List the name and product id of all product types. Each product type has a variety of different items with unique item ids and options. There are only 50 product types in the store.",
			InputSchema: schemaObject(nil, nil),
			Handler:     sierraListProductTypes,
		},
		// --- mutating tools ---
		"cancel_pending_order": {
			Name: "cancel_pending_order",
			Description: "Cancel a pending order. If the order is already processed or delivered, it cannot be cancelled. " +
				"The agent needs to explain the cancellation detail and ask for explicit user confirmation (yes/no) to proceed. " +
				"If the user confirms, the order status will be changed to 'cancelled' and the payment will be refunded. " +
				"The refund will be added to the user's gift card balance immediately if the payment was made using a gift card, " +
				"otherwise the refund would take 5-7 business days to process. The function returns the order details after the cancellation.",
			InputSchema: schemaObject(map[string]any{
				"order_id": schemaString("The order id, such as '#W0000000'. Be careful there is a '#' symbol at the beginning of the order id."),
				"reason":   schemaEnum([]string{"no longer needed", "ordered by mistake"}, "The reason for cancellation, which should be either 'no longer needed' or 'ordered by mistake'."),
			}, []string{"order_id", "reason"}),
			Handler: sierraCancelPendingOrder,
		},
		"exchange_delivered_order_items": {
			Name: "exchange_delivered_order_items",
			Description: "Exchange items in a delivered order to new items of the same product type. For a delivered order, " +
				"return or exchange can be only done once by the agent. The agent needs to explain the exchange detail and ask " +
				"for explicit user confirmation (yes/no) to proceed.",
			InputSchema: schemaObject(map[string]any{
				"order_id":          schemaString("The order id, such as '#W0000000'. Be careful there is a '#' symbol at the beginning of the order id."),
				"item_ids":          schemaStringList("The item ids to be exchanged, each such as '1008292230'. There could be duplicate items in the list."),
				"new_item_ids":      schemaStringList("The item ids to be exchanged for, each such as '1008292230'. There could be duplicate items in the list. Each new item id should match the item id in the same position and be of the same product."),
				"payment_method_id": schemaString("The payment method id to pay or receive refund for the item price difference, such as 'gift_card_0000000' or 'credit_card_0000000'. These can be looked up from the user or order details."),
			}, []string{"order_id", "item_ids", "new_item_ids", "payment_method_id"}),
			Handler: sierraExchangeDeliveredOrderItems,
		},
		"modify_pending_order_address": {
			Name:        "modify_pending_order_address",
			Description: "Modify the shipping address of a pending order. The agent needs to explain the modification detail and ask for explicit user confirmation (yes/no) to proceed.",
			InputSchema: schemaObject(map[string]any{
				"order_id": schemaString("The order id, such as '#W0000000'. Be careful there is a '#' symbol at the beginning of the order id."),
				"address1": schemaString("The first line of the address, such as '123 Main St'."),
				"address2": schemaString("The second line of the address, such as 'Apt 1' or ''."),
				"city":     schemaString("The city, such as 'San Francisco'."),
				"state":    schemaString("The state, such as 'CA'."),
				"country":  schemaString("The country, such as 'USA'."),
				"zip":      schemaString("The zip code, such as '12345'."),
			}, []string{"order_id", "address1", "address2", "city", "state", "country", "zip"}),
			Handler: sierraModifyPendingOrderAddress,
		},
		"modify_pending_order_items": {
			Name:        "modify_pending_order_items",
			Description: "Modify items in a pending order to new items of the same product type. For a pending order, this function can only be called once. The agent needs to explain the exchange detail and ask for explicit user confirmation (yes/no) to proceed.",
			InputSchema: schemaObject(map[string]any{
				"order_id":          schemaString("The order id, such as '#W0000000'. Be careful there is a '#' symbol at the beginning of the order id."),
				"item_ids":          schemaStringList("The item ids to be modified, each such as '1008292230'. There could be duplicate items in the list."),
				"new_item_ids":      schemaStringList("The item ids to be modified for, each such as '1008292230'. There could be duplicate items in the list. Each new item id should match the item id in the same position and be of the same product."),
				"payment_method_id": schemaString("The payment method id to pay or receive refund for the item price difference, such as 'gift_card_0000000' or 'credit_card_0000000'. These can be looked up from the user or order details."),
			}, []string{"order_id", "item_ids", "new_item_ids", "payment_method_id"}),
			Handler: sierraModifyPendingOrderItems,
		},
		"modify_pending_order_payment": {
			Name:        "modify_pending_order_payment",
			Description: "Modify the payment method of a pending order. The agent needs to explain the modification detail and ask for explicit user confirmation (yes/no) to proceed.",
			InputSchema: schemaObject(map[string]any{
				"order_id":          schemaString("The order id, such as '#W0000000'. Be careful there is a '#' symbol at the beginning of the order id."),
				"payment_method_id": schemaString("The payment method id to pay or receive refund for the item price difference, such as 'gift_card_0000000' or 'credit_card_0000000'. These can be looked up from the user or order details."),
			}, []string{"order_id", "payment_method_id"}),
			Handler: sierraModifyPendingOrderPayment,
		},
		"modify_user_address": {
			Name:        "modify_user_address",
			Description: "Modify the default address of a user. The agent needs to explain the modification detail and ask for explicit user confirmation (yes/no) to proceed.",
			InputSchema: schemaObject(map[string]any{
				"user_id":  schemaString("The user id, such as 'sara_doe_496'."),
				"address1": schemaString("The first line of the address, such as '123 Main St'."),
				"address2": schemaString("The second line of the address, such as 'Apt 1' or ''."),
				"city":     schemaString("The city, such as 'San Francisco'."),
				"state":    schemaString("The state, such as 'CA'."),
				"country":  schemaString("The country, such as 'USA'."),
				"zip":      schemaString("The zip code, such as '12345'."),
			}, []string{"user_id", "address1", "address2", "city", "state", "country", "zip"}),
			Handler: sierraModifyUserAddress,
		},
		"return_delivered_order_items": {
			Name:        "return_delivered_order_items",
			Description: "Return some items of a delivered order. The order status will be changed to 'return requested'. The agent needs to explain the return detail and ask for explicit user confirmation (yes/no) to proceed. The user will receive follow-up email for how and where to return the item.",
			InputSchema: schemaObject(map[string]any{
				"order_id":          schemaString("The order id, such as '#W0000000'. Be careful there is a '#' symbol at the beginning of the order id."),
				"item_ids":          schemaStringList("The item ids to be returned, each such as '1008292230'. There could be duplicate items in the list."),
				"payment_method_id": schemaString("The payment method id to pay or receive refund for the item price difference, such as 'gift_card_0000000' or 'credit_card_0000000'. These can be looked up from the user or order details."),
			}, []string{"order_id", "item_ids", "payment_method_id"}),
			Handler: sierraReturnDeliveredOrderItems,
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
			Name: "think",
			Description: "Use the tool to think about something. It will not obtain new information or change the database, " +
				"but just append the thought to the log. Use it when complex reasoning or some cache memory is needed.",
			InputSchema: schemaObject(map[string]any{
				"thought": schemaString("A thought to think about."),
			}, []string{"thought"}),
			Handler: sierraThink,
		},
		"transfer_to_human_agents": {
			Name: "transfer_to_human_agents",
			Description: "Transfer the user to a human agent, with a summary of the user's issue. " +
				"Only transfer if the user explicitly asks for a human agent, or if the user's issue cannot be resolved by the agent with the available tools.",
			InputSchema: schemaObject(map[string]any{
				"summary": schemaString("A summary of the user's issue."),
			}, []string{"summary"}),
			Handler: sierraTransferToHumanRetail,
		},
	}
}

// --- read handlers ---

func sierraGetOrderDetails(state State, args map[string]any) (any, error) {
	orderID, err := argString(args, "order_id")
	if err != nil {
		return nil, err
	}
	orders := asMap(state["orders"])
	o, ok := orders[orderID]
	if !ok {
		return "Error: order not found", nil
	}
	return jsonString(o)
}

func sierraGetProductDetails(state State, args map[string]any) (any, error) {
	productID, err := argString(args, "product_id")
	if err != nil {
		return nil, err
	}
	products := asMap(state["products"])
	p, ok := products[productID]
	if !ok {
		return "Error: product not found", nil
	}
	return jsonString(p)
}

func sierraGetUserDetails(state State, args map[string]any) (any, error) {
	userID, err := argString(args, "user_id")
	if err != nil {
		return nil, err
	}
	users := asMap(state["users"])
	u, ok := users[userID]
	if !ok {
		return "Error: user not found", nil
	}
	return jsonString(u)
}

func sierraFindUserByEmail(state State, args map[string]any) (any, error) {
	email, err := argString(args, "email")
	if err != nil {
		return nil, err
	}
	target := strings.ToLower(email)
	users := asMap(state["users"])
	// Sort keys for deterministic iteration; Python dict iteration
	// order matches insertion order, but in Sierra's data file the
	// order is also alphabetical so this matches.
	keys := make([]string, 0, len(users))
	for k := range users {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, uid := range keys {
		profile := asMap(users[uid])
		profEmail, _ := profile["email"].(string)
		if strings.ToLower(profEmail) == target {
			return uid, nil
		}
	}
	return "Error: user not found", nil
}

func sierraFindUserByNameZip(state State, args map[string]any) (any, error) {
	first, err := argString(args, "first_name")
	if err != nil {
		return nil, err
	}
	last, err := argString(args, "last_name")
	if err != nil {
		return nil, err
	}
	zip, err := argString(args, "zip")
	if err != nil {
		return nil, err
	}
	fLow := strings.ToLower(first)
	lLow := strings.ToLower(last)
	users := asMap(state["users"])
	keys := make([]string, 0, len(users))
	for k := range users {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, uid := range keys {
		profile := asMap(users[uid])
		name := asMap(profile["name"])
		addr := asMap(profile["address"])
		pf, _ := name["first_name"].(string)
		pl, _ := name["last_name"].(string)
		pz, _ := addr["zip"].(string)
		if strings.ToLower(pf) == fLow && strings.ToLower(pl) == lLow && pz == zip {
			return uid, nil
		}
	}
	return "Error: user not found", nil
}

func sierraListProductTypes(state State, _ map[string]any) (any, error) {
	products := asMap(state["products"])
	// Sierra builds {product["name"]: product["product_id"]} then
	// sorts alphabetically. Multiple products with the same name
	// would collide in Python's dict — we mirror that, last write
	// wins, then sort.
	nameToID := make(map[string]string, len(products))
	for _, v := range products {
		p := asMap(v)
		name, _ := p["name"].(string)
		pid, _ := p["product_id"].(string)
		nameToID[name] = pid
	}
	names := make([]string, 0, len(nameToID))
	for k := range nameToID {
		names = append(names, k)
	}
	sort.Strings(names)
	// Marshal as an ordered JSON object: encoding/json emits keys
	// sorted by ASCII; that matches Sierra's `dict(sorted(...))`
	// for the alphanumeric names in the fixture.
	out := make(map[string]string, len(nameToID))
	for _, k := range names {
		out[k] = nameToID[k]
	}
	return jsonString(out)
}

// --- mutating handlers ---

func sierraCancelPendingOrder(state State, args map[string]any) (any, error) {
	orderID, err := argString(args, "order_id")
	if err != nil {
		return nil, err
	}
	reason, err := argString(args, "reason")
	if err != nil {
		return nil, err
	}
	orders := asMap(state["orders"])
	o, ok := orders[orderID]
	if !ok {
		return "Error: order not found", nil
	}
	order := asMap(o)
	if order["status"] != "pending" {
		return "Error: non-pending order cannot be cancelled", nil
	}
	if reason != "no longer needed" && reason != "ordered by mistake" {
		return "Error: invalid reason", nil
	}
	users := asMap(state["users"])
	userID, _ := order["user_id"].(string)
	user := asMap(users[userID])
	pms := asMap(user["payment_methods"])

	history := asSlice(order["payment_history"])
	var refunds []any
	for _, pAny := range history {
		p := asMap(pAny)
		pmID, _ := p["payment_method_id"].(string)
		amt := asFloat(p["amount"])
		refund := map[string]any{
			"transaction_type":  "refund",
			"amount":            amt,
			"payment_method_id": pmID,
		}
		refunds = append(refunds, refund)
		if strings.Contains(pmID, "gift_card") {
			pm := asMap(pms[pmID])
			pm["balance"] = round2(asFloat(pm["balance"]) + amt)
		}
	}
	order["status"] = "cancelled"
	order["cancel_reason"] = reason
	order["payment_history"] = append(history, refunds...)
	return jsonString(order)
}

func sierraExchangeDeliveredOrderItems(state State, args map[string]any) (any, error) {
	orderID, err := argString(args, "order_id")
	if err != nil {
		return nil, err
	}
	itemIDs, err := argStringList(args, "item_ids")
	if err != nil {
		return nil, err
	}
	newItemIDs, err := argStringList(args, "new_item_ids")
	if err != nil {
		return nil, err
	}
	paymentMethodID, err := argString(args, "payment_method_id")
	if err != nil {
		return nil, err
	}
	products := asMap(state["products"])
	orders := asMap(state["orders"])
	users := asMap(state["users"])

	o, ok := orders[orderID]
	if !ok {
		return "Error: order not found", nil
	}
	order := asMap(o)
	if order["status"] != "delivered" {
		return "Error: non-delivered order cannot be exchanged", nil
	}

	items := asSlice(order["items"])
	allItemIDs := make([]string, 0, len(items))
	for _, it := range items {
		s, _ := asMap(it)["item_id"].(string)
		allItemIDs = append(allItemIDs, s)
	}
	for _, id := range itemIDs {
		if countString(itemIDs, id) > countString(allItemIDs, id) {
			return fmt.Sprintf("Error: %s not found", id), nil
		}
	}
	if len(itemIDs) != len(newItemIDs) {
		return "Error: the number of items to be exchanged should match", nil
	}

	var diffPrice float64
	for i := range itemIDs {
		var matched map[string]any
		for _, it := range items {
			m := asMap(it)
			if id, _ := m["item_id"].(string); id == itemIDs[i] {
				matched = m
				break
			}
		}
		if matched == nil {
			return fmt.Sprintf("Error: %s not found", itemIDs[i]), nil
		}
		productID, _ := matched["product_id"].(string)
		variants := asMap(asMap(products[productID])["variants"])
		variant := asMap(variants[newItemIDs[i]])
		if variant == nil {
			return fmt.Sprintf("Error: new item %s not found or available", newItemIDs[i]), nil
		}
		avail, _ := variant["available"].(bool)
		if !avail {
			return fmt.Sprintf("Error: new item %s not found or available", newItemIDs[i]), nil
		}
		diffPrice += asFloat(variant["price"]) - asFloat(matched["price"])
	}
	diffPrice = round2(diffPrice)

	userID, _ := order["user_id"].(string)
	pms := asMap(asMap(users[userID])["payment_methods"])
	pm, ok := pms[paymentMethodID]
	if !ok {
		return "Error: payment method not found", nil
	}
	pmMap := asMap(pm)
	if src, _ := pmMap["source"].(string); src == "gift_card" {
		if asFloat(pmMap["balance"]) < diffPrice {
			return "Error: insufficient gift card balance to pay for the price difference", nil
		}
	}

	sortedItem := append([]string(nil), itemIDs...)
	sort.Strings(sortedItem)
	sortedNew := append([]string(nil), newItemIDs...)
	sort.Strings(sortedNew)
	order["status"] = "exchange requested"
	order["exchange_items"] = stringsToAnys(sortedItem)
	order["exchange_new_items"] = stringsToAnys(sortedNew)
	order["exchange_payment_method_id"] = paymentMethodID
	order["exchange_price_difference"] = diffPrice
	return jsonString(order)
}

func sierraModifyPendingOrderAddress(state State, args map[string]any) (any, error) {
	orderID, err := argString(args, "order_id")
	if err != nil {
		return nil, err
	}
	addr1, err := argString(args, "address1")
	if err != nil {
		return nil, err
	}
	addr2, err := argString(args, "address2")
	if err != nil {
		return nil, err
	}
	city, err := argString(args, "city")
	if err != nil {
		return nil, err
	}
	st, err := argString(args, "state")
	if err != nil {
		return nil, err
	}
	country, err := argString(args, "country")
	if err != nil {
		return nil, err
	}
	zip, err := argString(args, "zip")
	if err != nil {
		return nil, err
	}
	orders := asMap(state["orders"])
	o, ok := orders[orderID]
	if !ok {
		return "Error: order not found", nil
	}
	order := asMap(o)
	if order["status"] != "pending" {
		return "Error: non-pending order cannot be modified", nil
	}
	order["address"] = map[string]any{
		"address1": addr1,
		"address2": addr2,
		"city":     city,
		"state":    st,
		"country":  country,
		"zip":      zip,
	}
	return jsonString(order)
}

func sierraModifyPendingOrderItems(state State, args map[string]any) (any, error) {
	orderID, err := argString(args, "order_id")
	if err != nil {
		return nil, err
	}
	itemIDs, err := argStringList(args, "item_ids")
	if err != nil {
		return nil, err
	}
	newItemIDs, err := argStringList(args, "new_item_ids")
	if err != nil {
		return nil, err
	}
	paymentMethodID, err := argString(args, "payment_method_id")
	if err != nil {
		return nil, err
	}
	products := asMap(state["products"])
	orders := asMap(state["orders"])
	users := asMap(state["users"])

	o, ok := orders[orderID]
	if !ok {
		return "Error: order not found", nil
	}
	order := asMap(o)
	if order["status"] != "pending" {
		return "Error: non-pending order cannot be modified", nil
	}
	items := asSlice(order["items"])
	allItemIDs := make([]string, 0, len(items))
	for _, it := range items {
		s, _ := asMap(it)["item_id"].(string)
		allItemIDs = append(allItemIDs, s)
	}
	for _, id := range itemIDs {
		if countString(itemIDs, id) > countString(allItemIDs, id) {
			return fmt.Sprintf("Error: %s not found", id), nil
		}
	}
	if len(itemIDs) != len(newItemIDs) {
		return "Error: the number of items to be exchanged should match", nil
	}
	var diffPrice float64
	for i := range itemIDs {
		var matched map[string]any
		for _, it := range items {
			m := asMap(it)
			if id, _ := m["item_id"].(string); id == itemIDs[i] {
				matched = m
				break
			}
		}
		if matched == nil {
			return fmt.Sprintf("Error: %s not found", itemIDs[i]), nil
		}
		productID, _ := matched["product_id"].(string)
		variants := asMap(asMap(products[productID])["variants"])
		variant := asMap(variants[newItemIDs[i]])
		if variant == nil {
			return fmt.Sprintf("Error: new item %s not found or available", newItemIDs[i]), nil
		}
		avail, _ := variant["available"].(bool)
		if !avail {
			return fmt.Sprintf("Error: new item %s not found or available", newItemIDs[i]), nil
		}
		diffPrice += asFloat(variant["price"]) - asFloat(matched["price"])
	}

	userID, _ := order["user_id"].(string)
	pms := asMap(asMap(users[userID])["payment_methods"])
	pm, ok := pms[paymentMethodID]
	if !ok {
		return "Error: payment method not found", nil
	}
	pmMap := asMap(pm)
	if src, _ := pmMap["source"].(string); src == "gift_card" && asFloat(pmMap["balance"]) < diffPrice {
		return "Error: insufficient gift card balance to pay for the new item", nil
	}

	history := asSlice(order["payment_history"])
	txType := "payment"
	if diffPrice < 0 {
		txType = "refund"
	}
	history = append(history, map[string]any{
		"transaction_type":  txType,
		"amount":            math.Abs(diffPrice),
		"payment_method_id": paymentMethodID,
	})
	order["payment_history"] = history
	if src, _ := pmMap["source"].(string); src == "gift_card" {
		pmMap["balance"] = round2(asFloat(pmMap["balance"]) - diffPrice)
	}

	for i := range itemIDs {
		var matched map[string]any
		for _, it := range items {
			m := asMap(it)
			if id, _ := m["item_id"].(string); id == itemIDs[i] {
				matched = m
				break
			}
		}
		if matched == nil {
			continue
		}
		productID, _ := matched["product_id"].(string)
		variants := asMap(asMap(products[productID])["variants"])
		variant := asMap(variants[newItemIDs[i]])
		matched["item_id"] = newItemIDs[i]
		matched["price"] = variant["price"]
		matched["options"] = variant["options"]
	}
	order["status"] = "pending (item modified)"
	return jsonString(order)
}

func sierraModifyPendingOrderPayment(state State, args map[string]any) (any, error) {
	orderID, err := argString(args, "order_id")
	if err != nil {
		return nil, err
	}
	paymentMethodID, err := argString(args, "payment_method_id")
	if err != nil {
		return nil, err
	}
	orders := asMap(state["orders"])
	o, ok := orders[orderID]
	if !ok {
		return "Error: order not found", nil
	}
	order := asMap(o)
	if order["status"] != "pending" {
		return "Error: non-pending order cannot be modified", nil
	}
	users := asMap(state["users"])
	userID, _ := order["user_id"].(string)
	pms := asMap(asMap(users[userID])["payment_methods"])
	if _, ok := pms[paymentMethodID]; !ok {
		return "Error: payment method not found", nil
	}
	history := asSlice(order["payment_history"])
	if len(history) > 1 {
		return "Error: there should be exactly one payment for a pending order", nil
	}
	if len(history) == 0 {
		return "Error: there should be exactly one payment for a pending order", nil
	}
	first := asMap(history[0])
	if tt, _ := first["transaction_type"].(string); tt != "payment" {
		return "Error: there should be exactly one payment for a pending order", nil
	}
	oldPMID, _ := first["payment_method_id"].(string)
	if oldPMID == paymentMethodID {
		return "Error: the new payment method should be different from the current one", nil
	}
	amount := asFloat(first["amount"])
	newPM := asMap(pms[paymentMethodID])
	if src, _ := newPM["source"].(string); src == "gift_card" && asFloat(newPM["balance"]) < amount {
		return "Error: insufficient gift card balance to pay for the order", nil
	}
	history = append(history,
		map[string]any{"transaction_type": "payment", "amount": amount, "payment_method_id": paymentMethodID},
		map[string]any{"transaction_type": "refund", "amount": amount, "payment_method_id": oldPMID},
	)
	order["payment_history"] = history
	if src, _ := newPM["source"].(string); src == "gift_card" {
		newPM["balance"] = round2(asFloat(newPM["balance"]) - amount)
	}
	if strings.Contains(oldPMID, "gift_card") {
		oldPM := asMap(pms[oldPMID])
		oldPM["balance"] = round2(asFloat(oldPM["balance"]) + amount)
	}
	return jsonString(order)
}

func sierraModifyUserAddress(state State, args map[string]any) (any, error) {
	userID, err := argString(args, "user_id")
	if err != nil {
		return nil, err
	}
	addr1, err := argString(args, "address1")
	if err != nil {
		return nil, err
	}
	addr2, err := argString(args, "address2")
	if err != nil {
		return nil, err
	}
	city, err := argString(args, "city")
	if err != nil {
		return nil, err
	}
	st, err := argString(args, "state")
	if err != nil {
		return nil, err
	}
	country, err := argString(args, "country")
	if err != nil {
		return nil, err
	}
	zip, err := argString(args, "zip")
	if err != nil {
		return nil, err
	}
	users := asMap(state["users"])
	u, ok := users[userID]
	if !ok {
		return "Error: user not found", nil
	}
	user := asMap(u)
	user["address"] = map[string]any{
		"address1": addr1,
		"address2": addr2,
		"city":     city,
		"state":    st,
		"country":  country,
		"zip":      zip,
	}
	return jsonString(user)
}

func sierraReturnDeliveredOrderItems(state State, args map[string]any) (any, error) {
	orderID, err := argString(args, "order_id")
	if err != nil {
		return nil, err
	}
	itemIDs, err := argStringList(args, "item_ids")
	if err != nil {
		return nil, err
	}
	paymentMethodID, err := argString(args, "payment_method_id")
	if err != nil {
		return nil, err
	}
	orders := asMap(state["orders"])
	o, ok := orders[orderID]
	if !ok {
		return "Error: order not found", nil
	}
	order := asMap(o)
	if order["status"] != "delivered" {
		return "Error: non-delivered order cannot be returned", nil
	}
	users := asMap(state["users"])
	userID, _ := order["user_id"].(string)
	pms := asMap(asMap(users[userID])["payment_methods"])
	if _, ok := pms[paymentMethodID]; !ok {
		return "Error: payment method not found", nil
	}
	history := asSlice(order["payment_history"])
	originalPMID, _ := asMap(history[0])["payment_method_id"].(string)
	if !strings.Contains(paymentMethodID, "gift_card") && paymentMethodID != originalPMID {
		return "Error: payment method should be either the original payment method or a gift card", nil
	}
	items := asSlice(order["items"])
	allItemIDs := make([]string, 0, len(items))
	for _, it := range items {
		s, _ := asMap(it)["item_id"].(string)
		allItemIDs = append(allItemIDs, s)
	}
	for _, id := range itemIDs {
		if countString(itemIDs, id) > countString(allItemIDs, id) {
			return "Error: some item not found", nil
		}
	}
	sortedItems := append([]string(nil), itemIDs...)
	sort.Strings(sortedItems)
	order["status"] = "return requested"
	order["return_items"] = stringsToAnys(sortedItems)
	order["return_payment_method_id"] = paymentMethodID
	return jsonString(order)
}

// --- aux handlers ---

func sierraCalculate(_ State, args map[string]any) (any, error) {
	expr, err := argString(args, "expression")
	if err != nil {
		return nil, err
	}
	v, err := safeEval(expr)
	if err != nil {
		return fmt.Sprintf("Error: %s", err.Error()), nil
	}
	// Sierra returns `str(round(..., 2))`; Python prints "44.08"
	// (trailing zero stripped only if integral). We match that.
	return fmt.Sprintf("%g", round2(v)), nil
}

func sierraThink(_ State, _ map[string]any) (any, error) {
	return "", nil
}

func sierraTransferToHumanRetail(_ State, _ map[string]any) (any, error) {
	return "Transfer successful", nil
}

// --- tiny schema helpers ---
//
// These return JSON-Schema fragments. They live here (not
// sierra_common.go) so each tool's registration call stays
// readable in one place.

func schemaString(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func schemaStringList(desc string) map[string]any {
	return map[string]any{
		"type":        "array",
		"items":       map[string]any{"type": "string"},
		"description": desc,
	}
}

func schemaEnum(values []string, desc string) map[string]any {
	enum := make([]any, len(values))
	for i, v := range values {
		enum[i] = v
	}
	return map[string]any{"type": "string", "enum": enum, "description": desc}
}

func schemaObject(props map[string]any, required []string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	if required == nil {
		required = []string{}
	}
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   anySlice(required),
	}
}

func anySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func stringsToAnys(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
