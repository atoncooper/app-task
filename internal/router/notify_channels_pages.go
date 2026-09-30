// Notify channel admin surface: console page (SSR) + /api/channels for the
// CLI — upsert-by-name CRUD + test-send. All behind admin. Channel config is
// write-only: the encrypted blob is never rendered back, and the cipher is
// nil without SECURITY__API_KEY_ENCRYPTION_KEY (page renders a warning,
// create/update fail loud).
package router

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"app-task/internal/model"
	"app-task/internal/repo"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const maxChannelCount = 100

// channelNameRe: it flows into logs, audit fields and the CLI; keep it tight
// (same charset discipline as secrets/node_id).
var channelNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,63}$`)

var channelTypeSet = map[string]bool{
	repo.ChannelTypeEmail:    true,
	repo.ChannelTypeDingTalk: true,
	repo.ChannelTypeFeishu:   true,
	repo.ChannelTypeWebhook:  true,
}

// channelRowView is the console view — config ciphertext never appears.
type channelRowView struct {
	Name, Type, TypeText, Enabled, EnabledBadge, UpdatedBy, UpdatedAt string
}

func (r *Router) pageChannels(c *gin.Context) {
	if _, ok := requireAdminPage(c); !ok {
		return
	}
	rows, err := repo.ListNotifyChannels()
	if err != nil {
		slog.Error("[PAGE] list channels failed", "err", err)
		http.Error(c.Writer, "查询失败", http.StatusInternalServerError)
		return
	}
	items := make([]channelRowView, 0, len(rows))
	for _, ch := range rows {
		items = append(items, channelRowView{
			Name:         ch.Name,
			Type:         ch.Type,
			TypeText:     channelTypeText(ch.Type),
			Enabled:      yesNo(ch.Enabled),
			EnabledBadge: badgeClass(ch.Enabled),
			UpdatedBy:    ch.UpdatedBy,
			UpdatedAt:    fmtTime(ch.UpdatedAt),
		})
	}
	renderPage(c.Writer, "channels", struct {
		BaseData
		Items      []channelRowView
		CipherOK   bool
		TypeKeys   []string
		TypeLabels map[string]string
	}{
		BaseData:   newBase(c, "channels", "通知渠道", "notify 任务按名称引用渠道投递（webhook 凭据加密存储，写入后不可查看）"),
		Items:      items,
		CipherOK:   r.cipher != nil,
		TypeKeys:   []string{repo.ChannelTypeEmail, repo.ChannelTypeDingTalk, repo.ChannelTypeFeishu, repo.ChannelTypeWebhook},
		TypeLabels: channelTypeLabels(),
	})
}

func channelTypeLabels() map[string]string {
	return map[string]string{
		repo.ChannelTypeEmail:    "邮件组",
		repo.ChannelTypeDingTalk: "钉钉机器人",
		repo.ChannelTypeFeishu:   "飞书机器人",
		repo.ChannelTypeWebhook:  "通用 Webhook（企微/自建）",
	}
}

func channelTypeText(t string) string { return channelTypeLabels()[t] }

func yesNo(b bool) string {
	if b {
		return "启用"
	}
	return "停用"
}

func badgeClass(b bool) string {
	if b {
		return "success"
	}
	return "default"
}

// validateChannelConfig enforces per-type required fields so a broken config
// fails at creation, not on the day the notification matters.
func validateChannelConfig(chType, configJSON string) error {
	if len(configJSON) > 8<<10 {
		return fmt.Errorf("config 过大（≤8KB）")
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return fmt.Errorf("config 不是合法 JSON：%v", err)
	}
	nonEmpty := func(key string) bool {
		s, _ := cfg[key].(string)
		return strings.TrimSpace(s) != ""
	}
	switch chType {
	case repo.ChannelTypeEmail:
		to, ok := cfg["to"].([]any)
		if !ok || len(to) == 0 {
			return fmt.Errorf(`email 渠道 config 需要 {"to":["a@x.com"]}`)
		}
	case repo.ChannelTypeDingTalk, repo.ChannelTypeFeishu:
		if !nonEmpty("webhook") {
			return fmt.Errorf("config.webhook 必填（机器人 webhook 地址）")
		}
	case repo.ChannelTypeWebhook:
		if !nonEmpty("url") {
			return fmt.Errorf("config.url 必填")
		}
		if !nonEmpty("body") {
			return fmt.Errorf(`config.body 必填（报文模板，如 {"msgtype":"markdown","markdown":{"content":"{{.text}}"}}）`)
		}
	}
	return nil
}

// persistChannel validates + encrypts + persists one channel (upsert by
// name). Returns created=true for a new row.
func (r *Router) persistChannel(name, chType, configJSON, operator string) (bool, error) {
	if r.cipher == nil {
		return false, fmt.Errorf("密钥库未启用：缺少 SECURITY__API_KEY_ENCRYPTION_KEY，渠道功能不可用")
	}
	if !channelNameRe.MatchString(name) {
		return false, fmt.Errorf("名称需为小写字母开头的 [a-z0-9_-]（2–64 位）")
	}
	if !channelTypeSet[chType] {
		return false, fmt.Errorf("渠道类型不合法：%s", chType)
	}
	if err := validateChannelConfig(chType, configJSON); err != nil {
		return false, err
	}
	enc, err := r.cipher.Encrypt(configJSON)
	if err != nil {
		return false, fmt.Errorf("加密失败：%w", err)
	}
	existing, err := repo.GetNotifyChannelByName(name)
	if err != nil {
		return false, fmt.Errorf("查询失败：%w", err)
	}
	if existing == nil {
		if count, _ := repo.CountNotifyChannels(); count >= maxChannelCount {
			return false, fmt.Errorf("渠道数量已达上限（%d）", maxChannelCount)
		}
		ch := &model.NotifyChannel{
			ChannelID: uuid.NewString(),
			Name:      name,
			Type:      chType,
			ConfigEnc: enc,
			Enabled:   true,
			CreatedBy: operator,
			UpdatedBy: operator,
		}
		if err := repo.CreateNotifyChannel(ch); err != nil {
			return false, fmt.Errorf("保存失败：%w", err)
		}
		return true, nil
	}
	existing.Type = chType
	existing.ConfigEnc = enc
	existing.UpdatedBy = operator
	if err := repo.UpdateNotifyChannel(existing); err != nil {
		return false, fmt.Errorf("更新失败：%w", err)
	}
	return false, nil
}

// handleChannelCreate/update are the page forms: create takes full fields,
// update reuses them for an existing name (upsert semantics — same name
// refreshes type/config; enabled stays untouched by the create form and is
// toggled per-row).
func (r *Router) handleChannelCreate(c *gin.Context) {
	s, ok := requireAdminPage(c)
	if !ok {
		return
	}
	created, err := r.persistChannel(strings.TrimSpace(c.PostForm("name")),
		strings.TrimSpace(c.PostForm("type")), strings.TrimSpace(c.PostForm("config")), s.Username)
	if err != nil {
		redirectFlash(c, "/console/channels", "err", "%v", err)
		return
	}
	if created {
		redirectFlash(c, "/console/channels", "ok", "渠道已创建：%s", c.PostForm("name"))
		return
	}
	redirectFlash(c, "/console/channels", "ok", "渠道配置已更新：%s", c.PostForm("name"))
}

func (r *Router) handleChannelToggle(c *gin.Context) {
	if _, ok := requireAdminPage(c); !ok {
		return
	}
	name := c.Param("name")
	row, err := repo.GetNotifyChannelByName(name)
	if err != nil || row == nil {
		redirectFlash(c, "/console/channels", "err", "渠道不存在：%s", name)
		return
	}
	row.Enabled = !row.Enabled
	if err := repo.UpdateNotifyChannel(row); err != nil {
		redirectFlash(c, "/console/channels", "err", "操作失败：%v", err)
		return
	}
	redirectFlash(c, "/console/channels", "ok", "渠道 %s 已%s", name, yesNo(row.Enabled))
}

func (r *Router) handleChannelDelete(c *gin.Context) {
	if _, ok := requireAdminPage(c); !ok {
		return
	}
	name := c.Param("name")
	ok, err := repo.DeleteNotifyChannel(name)
	if err != nil {
		redirectFlash(c, "/console/channels", "err", "删除失败：%v", err)
		return
	}
	if !ok {
		redirectFlash(c, "/console/channels", "err", "渠道不存在：%s", name)
		return
	}
	redirectFlash(c, "/console/channels", "ok", "渠道已删除：%s", name)
}

func (r *Router) handleChannelTest(c *gin.Context) {
	if _, ok := requireAdminPage(c); !ok {
		return
	}
	if err := r.notifySvc.TestChannel(c.Param("name")); err != nil {
		redirectFlash(c, "/console/channels", "err", "测试发送失败：%v", err)
		return
	}
	redirectFlash(c, "/console/channels", "ok", "测试消息已发送")
}

// ── /api/channels (CLI: master token / API key, admin only) ─────────────

func (r *Router) apiListChannels(c *gin.Context) {
	rows, err := repo.ListNotifyChannels()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": err.Error()})
		return
	}
	type ch struct {
		Name      string `json:"name"`
		Type      string `json:"type"`
		Enabled   bool   `json:"enabled"`
		UpdatedAt string `json:"updated_at"`
	}
	out := make([]ch, 0, len(rows))
	for _, row := range rows {
		out = append(out, ch{Name: row.Name, Type: row.Type, Enabled: row.Enabled,
			UpdatedAt: fmtTime(row.UpdatedAt)})
	}
	c.JSON(http.StatusOK, gin.H{"items": out, "total": len(out)})
}

func (r *Router) apiUpsertChannel(c *gin.Context) {
	operator := ""
	if s, ok := currentUser(c); ok {
		operator = s.Username
	}
	var req struct {
		Name   string `json:"name"`
		Type   string `json:"type"`
		Config string `json:"config"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "invalid request: " + err.Error()})
		return
	}
	created, err := r.persistChannel(strings.TrimSpace(req.Name), strings.TrimSpace(req.Type),
		strings.TrimSpace(req.Config), operator)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"name": req.Name, "created": created})
}

func (r *Router) apiDeleteChannel(c *gin.Context) {
	ok, err := repo.DeleteNotifyChannel(c.Param("name"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": err.Error()})
		return
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"detail": "channel not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

func (r *Router) apiTestChannel(c *gin.Context) {
	if r.notifySvc == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": "notify service not wired"})
		return
	}
	if err := r.notifySvc.TestChannel(c.Param("name")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"sent": true})
}
