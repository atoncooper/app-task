package service

// NotifyService executes task_type="notify": it renders the task payload as a
// notification and delivers it through a channel — either inline email (payload
// carries to/cc directly, no channel entity needed) or a named notify_channel
// row (email/dingtalk/feishu/webhook; config is decrypted per run and never
// logged). Recurring notifications are simply notify tasks on a cron schedule —
// the scheduler's atomic cron extension (invariant I5) plus the email guard
// give exactly-once enqueue for email; dingtalk/feishu/webhook remain
// at-least-once like every executor side effect.
//
// The handler lives in the service layer (NOT executor/) because delivery
// touches the repo (channel resolution, email enqueue) — the executor package
// must stay storage-free.

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
	"app-task/internal/security"
)

// Payload/render limits: notify payloads are operator-authored and flow into
// emails/webhooks — bounded so a fat payload cannot stall the scheduler worker.
const (
	notifyMaxPayloadBytes = 64 << 10 // task payload JSON cap
	notifyMaxBodyBytes    = 32 << 10 // rendered text/html cap
	notifyMaxSubjectRunes = 256      // rendered subject/title cap
	notifyMaxVarCount     = 20       // custom template vars
	notifyMaxVarKeyRunes  = 32
	notifyMaxVarValBytes  = 1 << 10
	notifyMaxRecipients   = 50 // per list (to/cc)
)

// reservedTemplateVars are the built-ins injected into every render; user
// vars must not shadow them (the render would silently change meaning).
var reservedTemplateVars = map[string]bool{
	"date": true, "time": true, "datetime": true, "task_id": true,
	"title": true, "text": true,
}

// ChannelEmail is the inline delivery channel (no channel entity required).
const ChannelEmail = "email"

// Notification is the rendered, channel-agnostic message.
type Notification struct {
	Title   string // email subject / dingtalk markdown title
	Text    string // email html / markdown or plain text body
	MsgType string // text | markdown (dingtalk honors it; feishu/email/webhook ignore)
	To      []string
	CC      []string
}

// NotifyService wires the notify task type to its delivery channels.
type NotifyService struct {
	email  *EmailService
	cipher *security.Cipher // nil = channel entities disabled (inline email still works)
	http   *notifyHTTP
	rl     *channelLimiter
}

// NewNotifyService builds the notify handler's backing service. cipher may be
// nil (encryption key unconfigured): named channels then fail loud, inline
// email keeps working. httpTimeout/ratePerMin <= 0 fall back to defaults.
func NewNotifyService(email *EmailService, cipher *security.Cipher, httpTimeout time.Duration, ratePerMin int) *NotifyService {
	return &NotifyService{
		email:  email,
		cipher: cipher,
		http:   newNotifyHTTP(httpTimeout),
		rl:     newChannelLimiter(ratePerMin),
	}
}

// Handler returns the executor.Handler for task_type="notify".
func (s *NotifyService) Handler() executor.Handler {
	return func(_ context.Context, task executor.Task) error {
		return s.execute(task)
	}
}

// TestChannel sends a sample message through the named channel (console/API
// "send test" button). It bypasses the dedup guard — it is not task-driven.
func (s *NotifyService) TestChannel(name string) error {
	ch, err := s.resolveChannel(name)
	if err != nil {
		return err
	}
	now := time.Now()
	notif := &Notification{
		Title: "app-task 测试通知",
		Text: fmt.Sprintf("来自 app-task 的测试消息（%s）。能收到说明渠道 %q 配置正确。",
			now.In(time.Local).Format("2006-01-02 15:04:05"), name),
		MsgType: "markdown",
	}
	if ch.Type == repo.ChannelTypeEmail {
		if s.email == nil {
			return fmt.Errorf("notify channel %q: email service unavailable", name)
		}
		_, err := s.email.Enqueue(addressListAny(ch.Config["to"]),
			addressListAny(ch.Config["cc"]), notif.Title, notif.Text, "")
		return err
	}
	var mask []string
	return s.deliverWebhookish(ch, notif, &mask)
}

// execute renders the payload and dispatches through the selected channel.
// Returned errors feed the scheduler's retry policy (exponential backoff).
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

	data, err := templateData(task, p)
	if err != nil {
		return err
	}
	title, err := renderTemplate(p, "subject", data)
	if err != nil {
		return err
	}
	if strings.TrimSpace(title) == "" {
		return fmt.Errorf("notify subject required")
	}
	if len([]rune(title)) > notifyMaxSubjectRunes {
		return fmt.Errorf("notify subject too long (%d runes, max %d)", len([]rune(title)), notifyMaxSubjectRunes)
	}
	bodyKey := "html"
	if _, ok := p["html"]; !ok {
		bodyKey = "body"
	}
	text, err := renderTemplate(p, bodyKey, data)
	if err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("notify body required (html or body)")
	}
	if len(text) > notifyMaxBodyBytes {
		return fmt.Errorf("notify body too large (%d bytes, max %d)", len(text), notifyMaxBodyBytes)
	}
	notif := &Notification{
		Title:   title,
		Text:    text,
		MsgType: strings.ToLower(str(p["msgtype"])),
	}

	// mask collects every secret-ish string seen this run (webhook URLs carry
	// access_token; sign secrets are credentials) — all log/error paths go
	// through maskMsg so nothing lands in task_log/script_run/slog.
	var mask []string
	maskMsg := func(msg string) string {
		for _, v := range mask {
			if v != "" {
				msg = strings.ReplaceAll(msg, v, "***")
			}
		}
		return msg
	}
	taskLog := func(msg string) {
		if task.LogSink != nil {
			task.LogSink(msg)
		}
	}

	name := channelOf(p)
	if name == "" || name == ChannelEmail {
		// Inline email mode (no channel entity): payload carries to/cc.
		to, err := addressList(p, "to", true)
		if err != nil {
			return err
		}
		cc, err := addressList(p, "cc", false)
		if err != nil {
			return err
		}
		notif.To, notif.CC = to, cc
		return s.enqueueEmailGuarded(notif, "notify:"+task.ID, taskLog, maskMsg)
	}

	ch, err := s.resolveChannel(name)
	if err != nil {
		return err
	}
	if ch.Type == repo.ChannelTypeEmail {
		// Channel-configured recipients; payload to/cc may append.
		extraTo, err := addressList(p, "to", false)
		if err != nil {
			return err
		}
		extraCC, err := addressList(p, "cc", false)
		if err != nil {
			return err
		}
		notif.To = append(addressListAny(ch.Config["to"]), extraTo...)
		notif.CC = append(addressListAny(ch.Config["cc"]), extraCC...)
		if len(notif.To) == 0 {
			return fmt.Errorf("notify channel %q: config.to required (or pass to in payload)", name)
		}
		if len(notif.To)+len(notif.CC) > notifyMaxRecipients {
			return fmt.Errorf("notify channel %q: too many recipients (max %d)", name, notifyMaxRecipients)
		}
		return s.enqueueEmailGuarded(notif, "notify:"+task.ID, taskLog, maskMsg)
	}
	if err := s.deliverWebhookish(ch, notif, &mask); err != nil {
		return fmt.Errorf("%s", maskMsg(err.Error()))
	}
	slog.Info("[NOTIFY] delivered", "task_id", task.ID, "channel", name, "type", ch.Type)
	taskLog(fmt.Sprintf("notification delivered via channel %q (%s)", name, ch.Type))
	return nil
}

// enqueueEmailGuarded runs the at-least-once guard and enqueues via the mail
// queue (delivery + retries are the email worker's job). An in-flight or
// delivered email with the same reference proves this trigger already
// enqueued; FAILED rows do not count — the task-level retry policy owns that
// path. ref == "" (channel test) skips the guard.
func (s *NotifyService) enqueueEmailGuarded(notif *Notification, ref string, taskLog func(string), maskMsg func(string) string) error {
	if ref != "" {
		active, err := repo.EmailReferenceActive(ref)
		if err != nil {
			return fmt.Errorf("notify dedup check: %w", err)
		}
		if active {
			slog.Info("[NOTIFY] duplicate enqueue suppressed (reference already active)", "reference", ref)
			taskLog("notification already enqueued for this task — at-least-once redelivery suppressed")
			return nil
		}
	}
	emailID, err := s.email.Enqueue(notif.To, notif.CC, notif.Title, notif.Text, ref)
	if err != nil {
		return fmt.Errorf("notify enqueue: %s", maskMsg(err.Error()))
	}
	slog.Info("[NOTIFY] queued", "email_id", emailID, "to", notif.To)
	taskLog(fmt.Sprintf("email queued: %s (to %s)", emailID, strings.Join(notif.To, ", ")))
	return nil
}

// resolveChannel loads the entity, enforces enabled + the rate limit, and
// decrypts the config blob.
func (s *NotifyService) resolveChannel(name string) (*channelRow, error) {
	if s.cipher == nil {
		return nil, fmt.Errorf("notify channel %q unavailable: encryption key not configured (SECURITY__API_KEY_ENCRYPTION_KEY)", name)
	}
	row, err := repo.GetNotifyChannelByName(name)
	if err != nil {
		return nil, fmt.Errorf("notify channel lookup: %w", err)
	}
	if row == nil {
		return nil, fmt.Errorf("notify channel %q not found (create it in the console)", name)
	}
	if !row.Enabled {
		return nil, fmt.Errorf("notify channel %q is disabled", name)
	}
	if !s.rl.allow(name) {
		return nil, fmt.Errorf("notify channel %q rate limited (%d/min) — retry next tick", name, s.rl.perMin)
	}
	plain, err := s.cipher.Decrypt(row.ConfigEnc)
	if err != nil {
		return nil, fmt.Errorf("notify channel %q decrypt: %w", name, err)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(plain), &cfg); err != nil {
		return nil, fmt.Errorf("notify channel %q config decode: %w", name, err)
	}
	return &channelRow{Name: row.Name, Type: row.Type, Config: cfg}, nil
}

// channelRow is a decrypted channel ready to deliver.
type channelRow struct {
	Name   string
	Type   string
	Config map[string]any
}

// channelOf reads the optional "channel" selector (default inline email).
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
// html/template: autoescaping would corrupt it).
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
	if len(raw) > notifyMaxRecipients {
		return nil, fmt.Errorf("notify %q too many recipients (%d, max %d)", key, len(raw), notifyMaxRecipients)
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

// str renders any JSON value as a trimmed string ("" when absent).
func str(v any) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

// DeliverByChannel renders notif through the named notify channel — the
// reuse path for digest summaries and failure alerts. Email-type channels
// enqueue with reference `ref` ("" skips the dedup guard); webhook-ish
// channels send directly. All channel machinery (resolve/decrypt/rate
// limit/mask) is shared with task-driven notify.
func (s *NotifyService) DeliverByChannel(name string, notif *Notification, ref string) error {
	ch, err := s.resolveChannel(name)
	if err != nil {
		return err
	}
	if ch.Type == repo.ChannelTypeEmail {
		notif.To = addressListAny(ch.Config["to"])
		notif.CC = addressListAny(ch.Config["cc"])
		if len(notif.To) == 0 {
			return fmt.Errorf("notify channel %q: config.to required", name)
		}
		return s.enqueueEmailGuarded(notif, ref, func(string) {}, func(m string) string { return m })
	}
	var mask []string
	return s.deliverWebhookish(ch, notif, &mask)
}
