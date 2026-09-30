package service

// Tests for the notify task type: payload validation, template rendering with
// built-in + custom vars, and the at-least-once enqueue guard (a reclaimed
// notify task must not enqueue a duplicate email).

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"app-task/internal/security"

	"app-task/internal/db"
	"app-task/internal/executor"
	"app-task/internal/model"
	"app-task/internal/repo"
)

func notifyHandler(t *testing.T) executor.Handler {
	t.Helper()
	setupTestDB(t)
	return NewNotifyService(NewEmailService(emailCfg()), nil, 0, 0).Handler()
}

func runNotify(t *testing.T, h executor.Handler, taskID, payload string, sink *[]string) error {
	t.Helper()
	task := executor.Task{ID: taskID, Payload: []byte(payload)}
	if sink != nil {
		task.LogSink = func(msg string) { *sink = append(*sink, msg) }
	}
	return h(nil, task)
}

func countNotifyEmails(t *testing.T, ref string) int64 {
	t.Helper()
	var n int64
	if err := db.DB.Model(&model.EmailMessage{}).Where("reference_id = ?", ref).Count(&n).Error; err != nil {
		t.Fatalf("count emails: %v", err)
	}
	return n
}

func TestNotifyRenderAndEnqueue(t *testing.T) {
	h := notifyHandler(t)
	var sink []string
	err := runNotify(t, h, "notify-task-1", `{
		"to": ["Ops 人 <ops@x.com>"], "cc": ["cc@x.com"],
		"subject": "日报 {{.date}}",
		"body": "<p>环境 {{.env}},task {{.task_id}}</p>",
		"vars": {"env": "prod"}
	}`, &sink)
	if err != nil {
		t.Fatalf("notify: %v", err)
	}
	var rows []model.EmailMessage
	db.DB.Where("reference_id = ?", "notify:notify-task-1").Find(&rows)
	if len(rows) != 1 {
		t.Fatalf("emails = %d, want 1", len(rows))
	}
	e := rows[0]
	if !strings.Contains(e.Subject, "-") || !strings.Contains(e.Subject, "日报") {
		t.Fatalf("subject not rendered with date: %q", e.Subject)
	}
	if !strings.Contains(e.BodyHTML, "环境 prod") || !strings.Contains(e.BodyHTML, "notify-task-1") {
		t.Fatalf("body not rendered: %q", e.BodyHTML)
	}
	if !strings.Contains(string(e.To), "ops@x.com") || !strings.Contains(string(e.CC), "cc@x.com") {
		t.Fatalf("addresses wrong: to=%s cc=%s", e.To, e.CC)
	}
	if len(sink) != 1 || !strings.Contains(sink[0], e.EmailID) {
		t.Fatalf("log sink = %v, want one line with email id", sink)
	}
}

func TestNotifyDuplicateSuppressed(t *testing.T) {
	h := notifyHandler(t)
	payload := `{"to":["a@x.com"],"subject":"s","body":"b"}`
	if err := runNotify(t, h, "notify-dup", payload, nil); err != nil {
		t.Fatal(err)
	}
	// Simulate the scheduler reclaiming and re-dispatching the same task id.
	if err := runNotify(t, h, "notify-dup", payload, nil); err != nil {
		t.Fatalf("redelivery must be suppressed, got: %v", err)
	}
	if n := countNotifyEmails(t, "notify:notify-dup"); n != 1 {
		t.Fatalf("emails = %d, want 1 (at-least-once guard failed)", n)
	}
}

func TestNotifyFailedEmailDoesNotBlockRetry(t *testing.T) {
	h := notifyHandler(t)
	payload := `{"to":["a@x.com"],"subject":"s","body":"b"}`
	if err := runNotify(t, h, "notify-fail", payload, nil); err != nil {
		t.Fatal(err)
	}
	// First delivery failed permanently (queue-level) — the notify task's own
	// retry policy may then re-run the handler: it must be allowed to enqueue
	// a fresh attempt.
	db.DB.Model(&model.EmailMessage{}).
		Where("reference_id = ?", "notify:notify-fail").
		Update("status", repo.StatusEmailFailed)
	if err := runNotify(t, h, "notify-fail", payload, nil); err != nil {
		t.Fatalf("re-enqueue after failure must be allowed: %v", err)
	}
	if n := countNotifyEmails(t, "notify:notify-fail"); n != 2 {
		t.Fatalf("emails = %d, want 2 (failed must not block task retry)", n)
	}
}

func TestNotifyValidation(t *testing.T) {
	h := notifyHandler(t)
	cases := []struct {
		name, payload, wantErr string
	}{
		{"missing to", `{"subject":"s","body":"b"}`, "missing"},
		{"bad address", `{"to":["not-an-email"],"subject":"s","body":"b"}`, "invalid email"},
		// named channel without a cipher: fail loud, never fall back silently
		{"channel without cipher", `{"channel":"dingtalk","subject":"s","body":"b"}`, "encryption key not configured"},
		{"empty subject", `{"to":["a@x.com"],"subject":"  ","body":"b"}`, "subject required"},
		{"empty body", `{"to":["a@x.com"],"subject":"s"}`, "body required"},
		{"bad template", `{"to":["a@x.com"],"subject":"{{.date","body":"b"}`, "template"},
		{"reserved var", `{"to":["a@x.com"],"subject":"s","body":"b","vars":{"date":"x"}}`, "shadows"},
		{"bad json", `{not json`, "decode"},
	}
	for _, c := range cases {
		if err := runNotify(t, h, "notify-val-"+c.name, c.payload, nil); err == nil {
			t.Errorf("%s: expected error", c.name)
		} else if !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: error = %q, want contains %q", c.name, err, c.wantErr)
		}
	}

	// With a cipher, an unknown channel resolves to "not found" (entity
	// lookup runs); the censored name must not leak config material.
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32))
	cipher, err := security.NewCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	hc := NewNotifyService(NewEmailService(emailCfg()), cipher, 0, 0).Handler()
	if err := runNotify(t, hc, "notify-val-unknown", `{"channel":"pagerduty","subject":"s","body":"b"}`, nil); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("unknown channel with cipher: err = %v, want not found", err)
	}
}
