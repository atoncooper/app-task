package service

// Channel senders for notify: email (via the mail queue), dingtalk and feishu
// group robots (webhook + optional HMAC-SHA256 signing), and a generic webhook
// (企业微信 robots / self-built endpoints). Two protocol subtleties both
// platforms share: HTTP status is 200 even on rejection — success is the JSON
// body's errcode/code == 0 — and the robot endpoints are credential-bearing
// URLs, so every error path runs through the caller's mask.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"text/template"
	"time"

	"app-task/internal/repo"
)

// notifyHTTP wraps the outbound client (timeout from notify.http_timeout_seconds).
type notifyHTTP struct{ c *http.Client }

func newNotifyHTTP(timeout time.Duration) *notifyHTTP {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &notifyHTTP{c: &http.Client{Timeout: timeout}}
}

// postJSON sends a JSON body and returns the response body; non-2xx is an
// error (the caller separately parses the body's errcode/code).
func (n *notifyHTTP) postJSON(endpoint string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	resp, err := n.c.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return data, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(data))
	}
	return data, nil
}

// postWithHeaders is postJSON for the generic webhook (custom headers, raw body).
func (n *notifyHTTP) postWithHeaders(endpoint string, headers map[string]string, contentType string, body []byte) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if contentType == "" {
		contentType = "application/json"
	}
	req.Header.Set("Content-Type", contentType)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := n.c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return data, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(data))
	}
	return data, nil
}

// deliverWebhookish dispatches the non-email channels (dingtalk/feishu/webhook).
func (s *NotifyService) deliverWebhookish(ch *channelRow, notif *Notification, mask *[]string) error {
	switch ch.Type {
	case repo.ChannelTypeDingTalk:
		return s.sendDingTalk(ch, notif, mask)
	case repo.ChannelTypeFeishu:
		return s.sendFeishu(ch, notif, mask)
	case repo.ChannelTypeWebhook:
		return s.sendWebhook(ch, notif, mask)
	default:
		return fmt.Errorf("notify channel %q: unknown type %q", ch.Name, ch.Type)
	}
}

// ── dingtalk ────────────────────────────────────────────────────────────
// config: {"webhook": "https://oapi.dingtalk.com/robot/send?access_token=...",
//          "sign_secret": "...",   // 加签机器人的 SEC 凭据;关键词机器人省略
//          "msgtype": "markdown"}  // 可选,默认 markdown

func (s *NotifyService) sendDingTalk(ch *channelRow, notif *Notification, mask *[]string) error {
	endpoint := str(ch.Config["webhook"])
	if endpoint == "" {
		return fmt.Errorf("notify channel %q: webhook required", ch.Name)
	}
	secret := str(ch.Config["sign_secret"])
	if secret != "" {
		ts := time.Now().UnixMilli()
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(fmt.Sprintf("%d\n%s", ts, secret)))
		sign := url.QueryEscape(base64.StdEncoding.EncodeToString(mac.Sum(nil)))
		endpoint = fmt.Sprintf("%s&timestamp=%d&sign=%s", endpoint, ts, sign)
		*mask = append(*mask, secret)
	}
	*mask = append(*mask, endpoint)

	msgtype := str(ch.Config["msgtype"])
	if msgtype == "" {
		msgtype = "markdown"
	}
	var payload map[string]any
	if msgtype == "text" {
		payload = map[string]any{"msgtype": "text", "text": map[string]string{
			"content": notif.Title + "\n" + notif.Text}}
	} else {
		payload = map[string]any{"msgtype": "markdown", "markdown": map[string]string{
			"title": notif.Title, "text": notif.Text}}
	}
	data, err := s.http.postJSON(endpoint, payload)
	if err != nil {
		return fmt.Errorf("dingtalk: %s (%s)", mask0(data, err), endpoint)
	}
	return checkRobotCode("dingtalk", data, "errcode", "errmsg")
}

// ── feishu ──────────────────────────────────────────────────────────────
// config: {"webhook": "https://open.feishu.cn/open-apis/bot/v2/hook/...",
//          "sign_secret": "..."}  // 加签机器人;签名体 = timestamp+"\n"+secret 作 HMAC key
// v1 报文只有 text（飞书 markdown 走 interactive 卡片,v2 再接）。

func (s *NotifyService) sendFeishu(ch *channelRow, notif *Notification, mask *[]string) error {
	endpoint := str(ch.Config["webhook"])
	if endpoint == "" {
		return fmt.Errorf("notify channel %q: webhook required", ch.Name)
	}
	*mask = append(*mask, endpoint)
	payload := map[string]any{"msg_type": "text", "content": map[string]string{
		"text": notif.Title + "\n" + notif.Text}}
	if secret := str(ch.Config["sign_secret"]); secret != "" {
		ts := time.Now().Unix()
		stringToSign := fmt.Sprintf("%d\n%s", ts, secret)
		mac := hmac.New(sha256.New, []byte(stringToSign))
		mac.Write([]byte("")) // 飞书约定:HMAC key 为 stringToSign,消息体为空
		payload["timestamp"] = fmt.Sprint(ts)
		payload["sign"] = base64.StdEncoding.EncodeToString(mac.Sum(nil))
		*mask = append(*mask, secret)
	}
	data, err := s.http.postJSON(endpoint, payload)
	if err != nil {
		return fmt.Errorf("feishu: %s", err)
	}
	if codeOK(data, "code") || codeOK(data, "StatusCode") {
		return nil
	}
	return fmt.Errorf("feishu: %s", string(data))
}

// checkRobotCode parses the dingtalk-shaped reply {"errcode":0,"errmsg":"ok"}.
func checkRobotCode(platform string, data []byte, codeKey, msgKey string) error {
	var reply map[string]any
	if err := json.Unmarshal(data, &reply); err != nil {
		return fmt.Errorf("%s: bad reply: %s", platform, string(data))
	}
	code, _ := reply[codeKey].(float64)
	if code != 0 {
		return fmt.Errorf("%s: %s=%v %s=%s", platform, codeKey, code, msgKey, reply[msgKey])
	}
	return nil
}

// codeOK reports whether the JSON body carries codeKey == 0.
func codeOK(data []byte, codeKey string) bool {
	var reply map[string]any
	if err := json.Unmarshal(data, &reply); err != nil {
		return false
	}
	code, _ := reply[codeKey].(float64)
	return code == 0
}

// mask0 stringifies an error, preferring any response body context.
func mask0(data []byte, err error) string {
	if len(data) > 0 {
		return string(data)
	}
	return err.Error()
}

// ── generic webhook (企业微信机器人 / 自建系统) ─────────────────────────
// config: {"url": "...", "body": "<模板,可用 {{.title}}/{{.text}} 与任务变量>",
//          "content_type": "application/json", "headers": {"Authorization": "..."}}

func (s *NotifyService) sendWebhook(ch *channelRow, notif *Notification, mask *[]string) error {
	endpoint := str(ch.Config["url"])
	if endpoint == "" {
		return fmt.Errorf("notify channel %q: url required", ch.Name)
	}
	*mask = append(*mask, endpoint)
	bodyTpl := str(ch.Config["body"])
	if bodyTpl == "" {
		return fmt.Errorf("notify channel %q: body template required", ch.Name)
	}
	tpl, err := template.New("notify.webhook").Parse(bodyTpl)
	if err != nil {
		return fmt.Errorf("notify channel %q body template: %w", ch.Name, err)
	}
	// 模板数据 = 任务变量 + title/text:企微机器人即
	// {"msgtype":"markdown","markdown":{"content":"{{.text}}"}}
	data := map[string]any{"title": notif.Title, "text": notif.Text}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("notify channel %q body render: %w", ch.Name, err)
	}
	if buf.Len() > notifyMaxBodyBytes {
		return fmt.Errorf("notify channel %q body too large", ch.Name)
	}
	headers := map[string]string{}
	if hv, ok := ch.Config["headers"].(map[string]any); ok {
		for k, v := range hv {
			headers[k] = fmt.Sprint(v)
			*mask = append(*mask, fmt.Sprint(v))
		}
	}
	if _, err := s.http.postWithHeaders(endpoint, headers, str(ch.Config["content_type"]), buf.Bytes()); err != nil {
		return fmt.Errorf("webhook: %s", err)
	}
	return nil
}

// addressListAny converts a config []any to clean strings (no email format
// validation here; malformed entries surface on delivery).
func addressListAny(raw any) []string {
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if v := strings.TrimSpace(fmt.Sprint(item)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// ── per-channel sliding-window rate limiter ─────────────────────────────

type channelLimiter struct {
	mu     sync.Mutex
	perMin int
	hits   map[string][]time.Time
}

func newChannelLimiter(perMin int) *channelLimiter {
	if perMin <= 0 {
		perMin = 18 // 钉钉机器人硬上限 20/min,留出余量
	}
	return &channelLimiter{perMin: perMin, hits: map[string][]time.Time{}}
}

func (l *channelLimiter) allow(name string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	fresh := l.hits[name][:0]
	for _, t := range l.hits[name] {
		if now.Sub(t) < time.Minute {
			fresh = append(fresh, t)
		}
	}
	if len(fresh) >= l.perMin {
		l.hits[name] = fresh
		return false
	}
	l.hits[name] = append(fresh, now)
	return true
}
