package service

// NotifyService executes task_type="notify": it renders the task payload as a
// notification and hands it to a delivery channel (email in this version;
// dingtalk/feishu/webhook land in the channel-entity milestone). Recurring
// notifications are simply notify tasks on a cron schedule — the scheduler's
// atomic cron extension (invariant I5) plus this handler's reference_id
// enqueue guard give exactly-once enqueue; delivery remains at-least-once via
// the mail queue's own claim/reclaim (invariant I4).
//
// The handler lives in the service layer (NOT executor/) because enqueueing
// touches the repo — the executor package must stay storage-free.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"text/template"
	"time"

	"app-task/internal/executor"
	"app-task/internal/repo"
)

// Payload/render limits: notify payloads are operator-authored and flow into
// emails — bounded so a fat payload cannot stall the scheduler worker or blow
// up the Resend request.
const (
	notifyMaxPayloadBytes = 64 << 10 // task payload JSON cap
	notifyMaxBodyBytes    = 32 << 10 // rendered html cap
	notifyMaxSubjectRunes = 256      // rendered subject cap
	notifyMaxVarCount     = 20       // custom template vars
	notifyMaxVarKeyRunes  = 32
	notifyMaxVarValBytes  = 1 << 10
)

// reservedTemplateVars are the built-ins injected into every render; user
// vars must not shadow them (the render would silently change meaning).
var reservedTemplateVars = map[string]bool{
	"date": true, "time": true, "datetime": true, "task_id": true,
}

// ChannelEmail is the only delivery channel in this milestone. PR-B adds
// dingtalk/feishu/webhook behind a Channel interface selected by payload
// "channel"; unknown values fail loud here until then.
const ChannelEmail = "email"

// NotifyService wires the notify task type to its delivery channel.
type NotifyService struct {
	email *EmailService
}

// NewNotifyService builds the notify executor handler's backing service.
func NewNotifyService(email *EmailService) *NotifyService {
	return &NotifyService{email: email}
}

// Handler returns the executor.Handler for task_type="notify".
func (s *NotifyService) Handler() executor.Handler {
	return func(_ context.Context, task executor.Task) error {
		return s.execute(task)
	}
}

// execute renders the payload and enqueues the notification. Any returned
// error feeds the scheduler's retry policy (exponential backoff).
func (s *NotifyService) execute(task executor.Task) error {
	if len(task.Payload) > notifyMaxPayloadBytes {
		return fmt.Errorf("notify payload too large (%d bytes, max %d)", len(task.Payload), notifyMaxPayloadBytes)
	}
	var p map[string]any
	if len(task.Payload) > 0 {
		if err := json.Unmarshal(task.Payload, &p); err != nil {
			return fmt.Errorf("notify payload decode: %w", err)
		}
	}

	switch channelOf(p) {
	case "", ChannelEmail:
		// only channel in this milestone
	default:
		return fmt.Errorf("unknown notify channel %q (available: email)", channelOf(p))
	}

	data, err := templateData(task, p)
	if err != nil {
		return err
	}
	subject, err := renderTemplate(p, "subject", data)
	if err != nil {
		return err
	}
	if strings.TrimSpace(subject) == "" {
		return fmt.Errorf("notify subject required")
	}
	if len([]rune(subject)) > notifyMaxSubjectRunes {
		return fmt.Errorf("notify subject too long (%d runes, max %d)", len([]rune(subject)), notifyMaxSubjectRunes)
	}
	bodyKey := "html"
	if _, ok := p["html"]; !ok {
		bodyKey = "body"
	}
	body, err := renderTemplate(p, bodyKey, data)
	if err != nil {
		return err
	}
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("notify body required (html or body)")
	}
	if len(body) > notifyMaxBodyBytes {
		return fmt.Errorf("notify body too large (%d bytes, max %d)", len(body), notifyMaxBodyBytes)
	}

	to, err := addressList(p, "to", true)
	if err != nil {
		return err
	}
	cc, err := addressList(p, "cc", false)
	if err != nil {
		return err
	}

	// At-least-once guard: a reclaimed notify task (dispatching TTL / dead
	// node) re-runs this handler; an in-flight or delivered email with the
	// same reference proves this trigger already enqueued. FAILED rows do not
	// count — the task-level retry policy owns that path.
	ref := "notify:" + task.ID
	if active, err := repo.EmailReferenceActive(ref); err != nil {
		return fmt.Errorf("notify dedup check: %w", err)
	} else if active {
		slog.Info("[NOTIFY] duplicate enqueue suppressed (reference already active)", "task_id", task.ID, "reference", ref)
		if task.LogSink != nil {
			task.LogSink("notification already enqueued for this task — at-least-once redelivery suppressed")
		}
		return nil
	}

	emailID, err := s.email.Enqueue(to, cc, subject, body, ref)
	if err != nil {
		return fmt.Errorf("notify enqueue: %w", err)
	}
	slog.Info("[NOTIFY] queued", "task_id", task.ID, "email_id", emailID, "to", to)
	if task.LogSink != nil {
		task.LogSink(fmt.Sprintf("email queued: %s (to %s)", emailID, strings.Join(to, ", ")))
	}
	return nil
}

// channelOf reads the optional "channel" selector (default email).
func channelOf(p map[string]any) string {
	if v, ok := p["channel"].(string); ok {
		return strings.ToLower(strings.TrimSpace(v))
	}
	return ""
}

// templateData merges the built-in vars (execution clock in the business
// timezone — time.Local is pinned by serve.go) with the payload's custom
// vars, rejecting reserved names instead of silently shadowing.
func templateData(task executor.Task, p map[string]any) (map[string]any, error) {
	now := time.Now()
	data := map[string]any{
		"date":     now.In(time.Local).Format("2006-01-02"),
		"time":     now.In(time.Local).Format("15:04:05"),
		"datetime": now.In(time.Local).Format("2006-01-02 15:04:05"),
		"task_id":  task.ID,
	}
	rawVars, ok := p["vars"].(map[string]any)
	if !ok {
		return data, nil
	}
	if len(rawVars) > notifyMaxVarCount {
		return nil, fmt.Errorf("notify vars too many (%d, max %d)", len(rawVars), notifyMaxVarCount)
	}
	for k, v := range rawVars {
		key := strings.TrimSpace(k)
		if key == "" || len([]rune(key)) > notifyMaxVarKeyRunes {
			return nil, fmt.Errorf("notify var name invalid: %q", k)
		}
		if reservedTemplateVars[key] {
			return nil, fmt.Errorf("notify var %q shadows a built-in template var", key)
		}
		val := fmt.Sprint(v)
		if len(val) > notifyMaxVarValBytes {
			return nil, fmt.Errorf("notify var %q too large (%d bytes, max %d)", key, len(val), notifyMaxVarValBytes)
		}
		data[key] = val
	}
	return data, nil
}

// renderTemplate renders the payload field as a text/template against data
// (body is operator-authored HTML/markdown — deliberately NOT
// html/template:autoescaping would corrupt it).
func renderTemplate(p map[string]any, key string, data map[string]any) (string, error) {
	raw, ok := p[key].(string)
	if !ok || raw == "" {
		return "", nil
	}
	tpl, err := template.New("notify." + key).Parse(raw)
	if err != nil {
		return "", fmt.Errorf("notify %s template: %w", key, err)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("notify %s render: %w", key, err)
	}
	return buf.String(), nil
}

// addressList extracts and validates a to/cc array (email format via
// net/mail so "Name <a@b.c>" forms work with the Resend provider).
func addressList(p map[string]any, key string, required bool) ([]string, error) {
	raw, ok := p[key].([]any)
	if !ok || len(raw) == 0 {
		if required {
			return nil, fmt.Errorf("notify payload missing %q address list", key)
		}
		return nil, nil
	}
	if len(raw) > 50 {
		return nil, fmt.Errorf("notify %q too many recipients (%d, max 50)", key, len(raw))
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		addr, err := mail.ParseAddress(strings.TrimSpace(fmt.Sprint(item)))
		if err != nil {
			return nil, fmt.Errorf("notify %q entry %q: invalid email address", key, item)
		}
		out = append(out, addr.Address)
	}
	return out, nil
}
