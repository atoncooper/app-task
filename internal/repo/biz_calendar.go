// Business calendar data access: named calendars + date overrides used by
// cron materialization to skip holidays (off) and fire adjusted-workday
// weekends (work).
package repo

import (
	"time"

	"app-task/internal/db"
	"app-task/internal/model"
)

// CreateBizCalendar inserts a new calendar (name unique).
func CreateBizCalendar(c *model.BizCalendar) error {
	return db.DB.Create(c).Error
}

// DeleteBizCalendar removes a calendar and its date overrides; false when
// absent. Tasks referencing the calendar keep their reference — cron
// materialization then fails loud per tick until the reference is fixed.
func DeleteBizCalendar(name string) (bool, error) {
	res := db.DB.Where("name = ?", name).Delete(&model.BizCalendar{})
	if res.Error != nil || res.RowsAffected == 0 {
		return res.RowsAffected > 0, res.Error
	}
	db.DB.Where("calendar_id = ?", name).Delete(&model.BizCalendarDate{})
	return true, nil
}

// ListBizCalendars returns calendars, name-ordered.
func ListBizCalendars() ([]model.BizCalendar, error) {
	var out []model.BizCalendar
	err := db.DB.Order("name ASC").Find(&out).Error
	return out, err
}

// GetBizCalendarByName resolves one calendar by name; nil, nil when absent.
func GetBizCalendarByName(name string) (*model.BizCalendar, error) {
	var c model.BizCalendar
	err := db.DB.Where("name = ?", name).First(&c).Error
	return &c, err
}

// CountBizCalendarDates counts a calendar's date overrides.
func CountBizCalendarDates(calendarID string) (int64, error) {
	var n int64
	err := db.DB.Model(&model.BizCalendarDate{}).Where("calendar_id = ?", calendarID).Count(&n).Error
	return n, err
}

// UpsertBizCalendarDate sets one date's override type (insert or update).
func UpsertBizCalendarDate(calendarID string, date time.Time, dayType string) error {
	var existing model.BizCalendarDate
	err := db.DB.Where("calendar_id = ? AND date = ?", calendarID, date).First(&existing).Error
	if err == nil {
		return db.DB.Model(&model.BizCalendarDate{}).Where("id = ?", existing.ID).
			Update("day_type", dayType).Error
	}
	return db.DB.Create(&model.BizCalendarDate{CalendarID: calendarID, Date: date, DayType: dayType}).Error
}

// DeleteBizCalendarDate removes one date override; false when absent.
func DeleteBizCalendarDate(calendarID string, date time.Time) (bool, error) {
	res := db.DB.Where("calendar_id = ? AND date = ?", calendarID, date).Delete(&model.BizCalendarDate{})
	return res.RowsAffected > 0, res.Error
}

// ListBizCalendarDates returns a calendar's overrides (whole set — calendars
// are small, a few hundred rows; dialect-agnostic filtering in Go).
func ListBizCalendarDates(calendarID string) ([]model.BizCalendarDate, error) {
	var out []model.BizCalendarDate
	err := db.DB.Where("calendar_id = ?", calendarID).Order("date ASC").Find(&out).Error
	return out, err
}
