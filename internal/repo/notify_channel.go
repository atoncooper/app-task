// Channel roster (notify_channel table): reusable notification delivery
// channels referenced by notify tasks via payload "channel". The config blob
// (webhook URL / signing secret / recipients) is stored AES-256-GCM encrypted;
// this package only moves ciphertext.
package repo

import (
	"errors"

	"app-task/internal/db"
	"app-task/internal/model"

	"gorm.io/gorm"
)

// Notify channel types (payload "channel" selects the sender; keep in sync
// with service/notify.go sender registry).
const (
	ChannelTypeEmail    = "email"
	ChannelTypeDingTalk = "dingtalk"
	ChannelTypeFeishu   = "feishu"
	ChannelTypeWebhook  = "webhook"
)

// ErrChannelNotFound is returned when a channel name does not resolve.
var ErrChannelNotFound = errors.New("notify channel not found")

// CreateNotifyChannel inserts a new channel (name must be unique).
func CreateNotifyChannel(ch *model.NotifyChannel) error {
	return db.DB.Create(ch).Error
}

// UpdateNotifyChannel refreshes config/audit fields by channel_id.
func UpdateNotifyChannel(ch *model.NotifyChannel) error {
	return db.DB.Model(&model.NotifyChannel{}).Where("channel_id = ?", ch.ChannelID).
		Updates(map[string]any{
			"type":       ch.Type,
			"config_enc": ch.ConfigEnc,
			"enabled":    ch.Enabled,
			"updated_by": ch.UpdatedBy,
		}).Error
}

// DeleteNotifyChannel removes a channel by name; false when absent.
func DeleteNotifyChannel(name string) (bool, error) {
	res := db.DB.Where("name = ?", name).Delete(&model.NotifyChannel{})
	return res.RowsAffected > 0, res.Error
}

// ListNotifyChannels returns the roster, name-ordered.
func ListNotifyChannels() ([]model.NotifyChannel, error) {
	var out []model.NotifyChannel
	err := db.DB.Order("name ASC").Find(&out).Error
	return out, err
}

// GetNotifyChannelByName resolves one channel by name; nil, nil when absent.
func GetNotifyChannelByName(name string) (*model.NotifyChannel, error) {
	var ch model.NotifyChannel
	err := db.DB.Where("name = ?", name).First(&ch).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	return &ch, err
}

// CountNotifyChannels counts the roster (console cap enforcement).
func CountNotifyChannels() (int64, error) {
	var n int64
	err := db.DB.Model(&model.NotifyChannel{}).Count(&n).Error
	return n, err
}
