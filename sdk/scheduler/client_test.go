package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

type recordingControl struct {
	rules []Rule
	once  []Once

	deletedNamespace  string
	deletedID         string
	canceledNamespace string
	canceledID        string
	putErr            error
	onceErr           error
}

func (c *recordingControl) PutRule(_ context.Context, rule Rule) error {
	c.rules = append(c.rules, rule)
	return c.putErr
}

func (c *recordingControl) DeleteRule(_ context.Context, namespace, id string) error {
	c.deletedNamespace, c.deletedID = namespace, id
	return nil
}

func (c *recordingControl) ListRules(context.Context, string) ([]Rule, error) {
	return append([]Rule(nil), c.rules...), nil
}

func (c *recordingControl) ScheduleOnce(_ context.Context, once Once) error {
	c.once = append(c.once, once)
	return c.onceErr
}

func (c *recordingControl) CancelOnce(_ context.Context, namespace, id string) error {
	c.canceledNamespace, c.canceledID = namespace, id
	return nil
}

func TestClientScopesIDsAndAbsoluteTimes(t *testing.T) {
	type job struct {
		Name string `json:"name"`
	}
	now := time.Date(2026, 8, 4, 3, 4, 5, 0, time.UTC)
	control := &recordingControl{}
	client, err := NewClient[job](
		control, "tenant-a", "job", 3,
		WithClientClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatal(err)
	}

	rule, err := client.PutRule(context.Background(), TypedRule[job]{
		Cron:     "0 * * * *",
		Timezone: "UTC",
		Task:     job{Name: "hourly"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rule.ID == "" || len(control.rules) != 1 {
		t.Fatalf("rule ID/control calls = %q/%d", rule.ID, len(control.rules))
	}
	if got := control.rules[0]; got.Namespace != "tenant-a" || got.ID != rule.ID ||
		got.Task.Payload.Kind != "job" || got.Task.Payload.Version != 3 {
		t.Fatalf("wire rule = %+v", got)
	}

	once, err := client.After(context.Background(), 2*time.Hour, job{Name: "later"})
	if err != nil {
		t.Fatal(err)
	}
	if once.ID == "" || !once.At.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("once = %+v", once)
	}
	if got := control.once[0]; got.Namespace != "tenant-a" || got.ID != once.ID || !got.At.Equal(once.At) {
		t.Fatalf("wire once = %+v", got)
	}

	at := now.Add(24 * time.Hour)
	if _, err := client.AtID(context.Background(), "fixed-id", at, job{Name: "tomorrow"}); err != nil {
		t.Fatal(err)
	}
	if got := control.once[1]; got.ID != "fixed-id" || !got.At.Equal(at) {
		t.Fatalf("fixed once = %+v", got)
	}

	if err := client.Cancel(context.Background(), once.ID); err != nil {
		t.Fatal(err)
	}
	if control.canceledNamespace != "tenant-a" || control.canceledID != once.ID {
		t.Fatalf("cancel scope = %q/%q", control.canceledNamespace, control.canceledID)
	}
	if err := client.Remove(context.Background(), rule.ID); err != nil {
		t.Fatal(err)
	}
	if control.deletedNamespace != "tenant-a" || control.deletedID != rule.ID {
		t.Fatalf("remove scope = %q/%q", control.deletedNamespace, control.deletedID)
	}

	list, err := client.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Task.Name != "hourly" {
		t.Fatalf("typed list = %+v", list)
	}
}

func TestClientReusesCallerID(t *testing.T) {
	type job struct {
		N int `json:"n"`
	}
	control := &recordingControl{}
	client, err := NewClient[job](control, "ns", "job", 1)
	if err != nil {
		t.Fatal(err)
	}
	input := TypedRule[job]{ID: "request-1", Cron: "* * * * *", Timezone: "UTC", Task: job{N: 1}}
	for range 2 {
		if _, err := client.PutRule(context.Background(), input); err != nil {
			t.Fatal(err)
		}
	}
	if control.rules[0].ID != "request-1" || control.rules[1].ID != "request-1" {
		t.Fatalf("IDs changed across retries: %+v", control.rules)
	}
}

func TestClientReturnsGeneratedRuleIDAfterLostResponse(t *testing.T) {
	type job struct {
		N int `json:"n"`
	}
	lost := errdefs.Timeoutf("response lost")
	control := &recordingControl{putErr: lost}
	client, err := NewClient[job](control, "ns", "job", 1)
	if err != nil {
		t.Fatal(err)
	}
	input := TypedRule[job]{Cron: "* * * * *", Timezone: "UTC", Task: job{N: 1}}
	partial, err := client.PutRule(context.Background(), input)
	if !errors.Is(err, lost) || partial.ID == "" || partial.Namespace != "ns" {
		t.Fatalf("PutRule partial/error = (%+v, %v)", partial, err)
	}
	control.putErr = nil
	retried, err := client.PutRule(context.Background(), partial)
	if err != nil {
		t.Fatal(err)
	}
	if retried.ID != partial.ID || len(control.rules) != 2 || control.rules[0].ID != control.rules[1].ID {
		t.Fatalf("retry IDs = partial %q, calls %+v", partial.ID, control.rules)
	}
}

func TestClientReturnsGeneratedOnceIDAfterLostResponse(t *testing.T) {
	type job struct {
		N int `json:"n"`
	}
	lost := errdefs.Timeoutf("response lost")
	control := &recordingControl{onceErr: lost}
	client, err := NewClient[job](control, "ns", "job", 1)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 4, 3, 0, 0, 0, time.UTC)
	partial, err := client.At(context.Background(), at, job{N: 1})
	if !errors.Is(err, lost) || partial.ID == "" || partial.Namespace != "ns" {
		t.Fatalf("At partial/error = (%+v, %v)", partial, err)
	}
	control.onceErr = nil
	retried, err := client.AtID(context.Background(), partial.ID, partial.At, job{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	if retried.ID != partial.ID || len(control.once) != 2 || control.once[0].ID != control.once[1].ID {
		t.Fatalf("retry IDs = partial %q, calls %+v", partial.ID, control.once)
	}
}

func TestNewClientRejectsTypedNilControl(t *testing.T) {
	var control *recordingControl
	if _, err := NewClient[string](control, "ns", "kind", 1); !errdefs.IsValidation(err) {
		t.Fatalf("NewClient error = %v, want validation", err)
	}
}

type selfValidatingTask struct {
	Err error    `json:"-"`
	Bad chan int `json:"bad"`
}

func (task selfValidatingTask) Validate() error { return task.Err }

func TestClientValidatesTaskBeforeEncoding(t *testing.T) {
	validateErr := errors.New("adapter task invalid")
	control := &recordingControl{}
	client, err := NewClient[selfValidatingTask](control, "ns", "kind", 1)
	if err != nil {
		t.Fatal(err)
	}
	task := selfValidatingTask{Err: validateErr, Bad: make(chan int)}
	if _, err := client.PutRule(context.Background(), TypedRule[selfValidatingTask]{
		ID: "rule", Cron: "* * * * *", Task: task,
	}); !errors.Is(err, validateErr) || !errdefs.IsValidation(err) {
		t.Fatalf("PutRule error = %v, want task validation", err)
	}
	if _, err := client.AtID(context.Background(), "once", time.Now(), task); !errors.Is(err, validateErr) ||
		!errdefs.IsValidation(err) {
		t.Fatalf("AtID error = %v, want task validation", err)
	}
	if len(control.rules) != 0 || len(control.once) != 0 {
		t.Fatal("invalid task reached Control")
	}
}
