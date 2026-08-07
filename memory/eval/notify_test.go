package main

import (
	"context"
	"testing"
)

func TestBuildNotifierRouting(t *testing.T) {
	if _, ok := buildNotifier("dry", true).(loggerNotifier); !ok {
		t.Fatal("dry-run should build loggerNotifier")
	}
	t.Setenv("FEISHU_APP_ID", "")
	t.Setenv("FEISHU_APP_SECRET", "")
	t.Setenv("FEISHU_CHAT_ID", "")
	if _, ok := buildNotifier("none", false).(noopNotifier); !ok {
		t.Fatal("missing credentials should build noopNotifier")
	}
	t.Setenv("FEISHU_APP_ID", "cli_test")
	t.Setenv("FEISHU_APP_SECRET", "secret")
	t.Setenv("FEISHU_CHAT_ID", "oc_test")
	if _, ok := buildNotifier("feishu", false).(*feishuApp); !ok {
		t.Fatal("full credentials should build feishuApp")
	}
}

func TestLoggerNotifierWritesWithoutError(t *testing.T) {
	notifier := loggerNotifier{name: "test"}
	if err := notifier.Notify(context.Background(), notifyEvent{
		Kind: "done", Title: "finished", Body: "n=1 qa.em=0",
	}); err != nil {
		t.Fatal(err)
	}
}
