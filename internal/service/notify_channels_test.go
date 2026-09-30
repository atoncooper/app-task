package service

// Channel sender tests: HMAC signing (dingtalk/feishu), the errcode/code
// body-parsing trap (both platforms answer HTTP 200 on rejection), the
// generic webhook template path, and a channel-entity end-to-end run.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"app-task/internal/model"
	"app-task/internal/repo"
	"app-task/internal/security"
)

func testCipher(t *testing.T) *security.Cipher {
	t.Helper()
	c, err := security.NewCipher(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	return c
}

// mustChannel inserts an enabled channel with an encrypted config.
func mustChannel(t *testing.T, name, chType, configJSON string, cipher *security.Cipher) {
	t.Helper()
	enc, err := cipher.Encrypt(configJSON)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := repo.CreateNotifyChannel(&model.NotifyChannel{
		ChannelID: name + "-id", Name: name, Type: chType, ConfigEnc: enc, Enabled: true,
		CreatedBy: "itest", UpdatedBy: "itest",
	}); err != nil {
		t.Fatalf("create channel: %v", err)
	}
}

func TestDingTalkSendSigned(t *testing.T) {
	setupTestDB(t)
	var gotPath, gotQuery, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer srv.Close()

	h := NewNotifyService(NewEmailService(emailCfg()), testCipher(t), 0, 0).Handler()
	mustChannel(t, "ops-ding", repo.ChannelTypeDingTalk,
		`{"webhook":"`+srv.URL+`/robot/send?access_token=abc","sign_secret":"SEC0001"}`, testCipher(t))
	err := runNotify(t, h, "notify-ding-1", `{
		"channel":"ops-ding","subject":"日报 {{.date}}","body":"**内容 {{.env}}**","vars":{"env":"prod"}
	}`, nil)
	if err != nil {
		t.Fatalf("dingtalk send: %v", err)
	}
	if gotPath != "/robot/send" {
		t.Fatalf("path = %s", gotPath)
	}
	for _, key := range []string{"timestamp=", "sign="} {
		if !strings.Contains(gotQuery, key) {
			t.Fatalf("signed query missing %s: %s", key, gotQuery)
		}
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("body: %v (%s)", err, gotBody)
	}
	if sent["msgtype"] != "markdown" {
		t.Fatalf("msgtype = %v", sent["msgtype"])
	}
	md := sent["markdown"].(map[string]any)
	if !strings.Contains(md["text"].(string), "**内容 prod**") {
		t.Fatalf("markdown text not rendered: %v", md["text"])
	}

	// The rejection trap: HTTP 200 with errcode != 0 must be a FAILURE.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errcode":310000,"errmsg":"sign not match"}`))
	}))
	defer srv2.Close()
	h2 := NewNotifyService(NewEmailService(emailCfg()), testCipher(t), 0, 0).Handler()
	mustChannel(t, "ops-ding-bad", repo.ChannelTypeDingTalk,
		`{"webhook":"`+srv2.URL+`"}`, testCipher(t))
	if err := runNotify(t, h2, "notify-ding-2", `{"channel":"ops-ding-bad","subject":"s","body":"b"}`, nil); err == nil ||
		!strings.Contains(err.Error(), "310000") {
		t.Fatalf("errcode 310000 must fail the task, got %v", err)
	}
}

func TestFeishuSendSigned(t *testing.T) {
	setupTestDB(t)
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"code":0,"msg":"success"}`))
	}))
	defer srv.Close()

	h := NewNotifyService(NewEmailService(emailCfg()), testCipher(t), 0, 0).Handler()
	mustChannel(t, "ops-feishu", repo.ChannelTypeFeishu,
		`{"webhook":"`+srv.URL+`/hook/x","sign_secret":"SECfs"}`, testCipher(t))
	if err := runNotify(t, h, "notify-fs-1", `{"channel":"ops-feishu","subject":"发布","body":"v1.2 上线"}`, nil); err != nil {
		t.Fatalf("feishu send: %v", err)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("body: %v (%s)", err, gotBody)
	}
	if sent["msg_type"] != "text" || sent["timestamp"] == nil || sent["sign"] == nil {
		t.Fatalf("feishu payload = %v (want text + timestamp + sign)", sent)
	}
	content := sent["content"].(map[string]any)["text"].(string)
	if !strings.Contains(content, "发布") || !strings.Contains(content, "v1.2 上线") {
		t.Fatalf("content = %v", content)
	}

	// Verify the feishu signing rule: sign = base64(hmac_sha256(key=ts+"\n"+secret, msg="")).
	ts := sent["timestamp"].(string)
	mac := hmac.New(sha256.New, []byte(ts+"\nSECfs"))
	mac.Write([]byte(""))
	if sent["sign"] != base64.StdEncoding.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("feishu sign mismatch: %v", sent["sign"])
	}
}

func TestWebhookChannelGeneric(t *testing.T) {
	setupTestDB(t)
	var gotHeaders, gotBody, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := NewNotifyService(NewEmailService(emailCfg()), testCipher(t), 0, 0).Handler()
	mustChannel(t, "ops-wecom", repo.ChannelTypeWebhook,
		`{"url":"`+srv.URL+`","headers":{"Authorization":"Bearer whsec-1"},
		  "body":"{\"msgtype\":\"markdown\",\"markdown\":{\"content\":\"{{.title}}|{{.text}}\"}}"}`,
		testCipher(t))
	if err := runNotify(t, h, "notify-wh-1", `{"channel":"ops-wecom","subject":"T1","body":"正文"}`, nil); err != nil {
		t.Fatalf("webhook send: %v", err)
	}
	if gotHeaders != "Bearer whsec-1" || !strings.Contains(gotCT, "application/json") {
		t.Fatalf("headers wrong: auth=%s ct=%s", gotHeaders, gotCT)
	}
	if !strings.Contains(gotBody, "T1|正文") {
		t.Fatalf("webhook body not rendered: %s", gotBody)
	}
}

func TestNotifyChannelEndToEndMasking(t *testing.T) {
	setupTestDB(t)
	// The webhook URL carries an access_token: a failed delivery must surface
	// "***" in the error, never the credential.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	secretURL := srv.URL + "/robot/send?access_token=SUPERSECRET"
	h := NewNotifyService(NewEmailService(emailCfg()), testCipher(t), 0, 0).Handler()
	mustChannel(t, "ops-mask", repo.ChannelTypeDingTalk, `{"webhook":"`+secretURL+`"}`, testCipher(t))
	err := runNotify(t, h, "notify-mask-1", `{"channel":"ops-mask","subject":"s","body":"b"}`, nil)
	if err == nil {
		t.Fatal("expected delivery failure")
	}
	if strings.Contains(err.Error(), "SUPERSECRET") || strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("credential leaked in error: %v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Fatalf("expected masked error, got: %v", err)
	}
}

func TestChannelRateLimiter(t *testing.T) {
	rl := newChannelLimiter(3)
	for i := 0; i < 3; i++ {
		if !rl.allow("x") {
			t.Fatalf("send %d unexpectedly limited", i+1)
		}
	}
	if rl.allow("x") {
		t.Fatal("4th send within window must be limited")
	}
	if !rl.allow("other") {
		t.Fatal("other channel must be independent")
	}
}
