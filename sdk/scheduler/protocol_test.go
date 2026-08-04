package scheduler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestProtocolJSONFields(t *testing.T) {
	now := time.Date(2026, 8, 4, 3, 0, 0, 0, time.UTC)
	d := Delivery{
		ID:          "delivery-1",
		ExecutionID: "execution-1",
		Namespace:   "memory",
		ScheduleID:  "daily",
		Task:        Task{Payload: Payload{Kind: "compact", Version: 1, Data: json.RawMessage(`{"days":7}`)}},
		Attempt:     2,
		LeaseToken:  "lease-1",
		LeaseUntil:  now.Add(time.Minute),
		ScheduledAt: now,
	}
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"id", "executionId", "namespace", "scheduleId", "task", "attempt",
		"leaseToken", "leaseUntil", "scheduledAt",
	} {
		if _, ok := fields[field]; !ok {
			t.Errorf("Delivery JSON missing %q: %s", field, data)
		}
	}
}

func TestProtocolValidationIsClassified(t *testing.T) {
	validTask := Task{Payload: Payload{Kind: "job", Version: 1, Data: json.RawMessage(`{}`)}}
	cases := []struct {
		name string
		err  error
	}{
		{"payload", Payload{}.Validate()},
		{"task", (Task{}).Validate()},
		{"rule", (Rule{Namespace: "ns", ID: "id", Cron: "* * * * *", Timezone: "UTC", Overlap: "bad", Task: validTask}).Validate()},
		{"once", (Once{Namespace: "ns", ID: "id", Task: validTask}).Validate()},
		{"delivery", (Delivery{}).Validate()},
		{"execution", (Execution{}).Validate()},
		{"claim", (ClaimRequest{}).Validate()},
		{"renew", (RenewRequest{}).Validate()},
		{"complete", (CompleteRequest{ExecutionID: "e", LeaseToken: "l", Status: StatusRunning}).Validate()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err == nil || !errdefs.IsValidation(tc.err) {
				t.Fatalf("error = %v, want validation classification", tc.err)
			}
		})
	}
}

func TestRuleAllowsEmptyTimezoneForUTCDefault(t *testing.T) {
	rule := Rule{
		Namespace: "ns",
		ID:        "daily",
		Cron:      "0 0 * * *",
		Task: Task{Payload: Payload{
			Kind: "job", Version: 1, Data: json.RawMessage(`{}`),
		}},
	}
	if err := rule.Validate(); err != nil {
		t.Fatalf("Validate empty timezone: %v", err)
	}
}

func TestStatusHelpers(t *testing.T) {
	for _, status := range []Status{StatusQueued, StatusRunning} {
		if !status.Outstanding() || status.Terminal() {
			t.Errorf("%q should be outstanding only", status)
		}
	}
	for _, status := range []Status{StatusSucceeded, StatusFailed, StatusCanceled} {
		if status.Outstanding() || !status.Terminal() {
			t.Errorf("%q should be terminal only", status)
		}
	}
	if Status("unknown").Outstanding() || Status("unknown").Terminal() {
		t.Fatal("unknown status must not be classified")
	}
}

func TestCompleteRequestRetainUntilValidationAndJSON(t *testing.T) {
	retainUntil := time.Date(2026, 8, 4, 5, 0, 0, 0, time.UTC)
	request := CompleteRequest{
		ExecutionID: "execution-1",
		LeaseToken:  "lease-1",
		Status:      StatusSucceeded,
		RetainUntil: &retainUntil,
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["retainUntil"]; !ok {
		t.Fatalf("CompleteRequest JSON missing retainUntil: %s", data)
	}

	nonUTC := time.Date(2026, 8, 4, 6, 0, 0, 0, time.FixedZone("west", -7*60*60))
	request.RetainUntil = &nonUTC
	if err := request.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("non-UTC RetainUntil validation = %v", err)
	}
}

func TestStrictJSONCodec(t *testing.T) {
	type job struct {
		Name string `json:"name"`
	}
	payload, err := NewJSONPayload("job", 2, job{Name: "index"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeJSON[job](payload, "job", 2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "index" {
		t.Fatalf("decoded job = %+v", got)
	}

	for _, tc := range []struct {
		name    string
		payload Payload
		kind    string
		version int
	}{
		{"kind", payload, "other", 2},
		{"version", payload, "job", 3},
		{"unknown", Payload{Kind: "job", Version: 2, Data: json.RawMessage(`{"name":"x","extra":true}`)}, "job", 2},
		{"trailing", Payload{Kind: "job", Version: 2, Data: json.RawMessage(`{"name":"x"} {}`)}, "job", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeJSON[job](tc.payload, tc.kind, tc.version); err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("DecodeJSON error = %v, want validation", err)
			}
		})
	}
}
