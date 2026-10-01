// Business calendar admin surface: console page (SSR) + /api/calendars for
// the CLI — calendar CRUD + date overrides. Admin only. Tasks reference
// calendars by name; deleting a calendar leaves those references dangling —
// cron materialization then fails loud per tick until the task is re-pointed.
package router

import (
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"app-task/internal/auth"
	"app-task/internal/dto"
	"app-task/internal/model"
	"app-task/internal/repo"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const maxCalendarCount = 50

var calendarNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,63}$`)

func (r *Router) pageCalendars(c *gin.Context) {
	if _, ok := requireAdminPage(c); !ok {
		return
	}
	cals, err := repo.ListBizCalendars()
	if err != nil {
		slog.Error("[PAGE] list calendars failed", "err", err)
		http.Error(c.Writer, "查询失败", http.StatusInternalServerError)
		return
	}
	type calRow struct {
		Name, Description, Dates, UpdatedAt string
	}
	rows := make([]calRow, 0, len(cals))
	for _, cal := range cals {
		n, _ := repo.CountBizCalendarDates(cal.CalendarID)
		rows = append(rows, calRow{
			Name:        cal.Name,
			Description: cal.Description,
			Dates:       fmt.Sprintf("%d", n),
			UpdatedAt:   fmtTime(cal.UpdatedAt),
		})
	}

	// selected calendar's dates
	selected := strings.TrimSpace(c.Query("calendar"))
	type dateRow struct {
		Date, DayType, DayTypeText string
	}
	var dates []dateRow
	if selected != "" {
		overrides, _ := repo.ListBizCalendarDates(selected)
		for _, d := range overrides {
			t := "休（跳过）"
			if d.DayType == model.CalendarDayWork {
				t = "班（补班触发）"
			}
			dates = append(dates, dateRow{Date: d.Date.Format("2006-01-02"), DayType: d.DayType, DayTypeText: t})
		}
	}

	renderPage(c.Writer, "bizcalendars", struct {
		BaseData
		Items      []calRow
		Selected   string
		Dates      []dateRow
		TypeLabels map[string]string
	}{
		BaseData:   newBase(c, "bizcalendars", "业务日历", "cron 物化时跳过节假日（休）、在调休周末触发（班）；任务创建时选择日历"),
		Items:      rows,
		Selected:   selected,
		Dates:      dates,
		TypeLabels: map[string]string{model.CalendarDayOff: "off", model.CalendarDayWork: "work"},
	})
}

func (r *Router) handleCalendarCreate(c *gin.Context) {
	s, ok := requireAdminPage(c)
	if !ok {
		return
	}
	name := strings.TrimSpace(c.PostForm("name"))
	if !calendarNameRe.MatchString(name) {
		redirectFlash(c, "/console/calendars", "err", "名称需为小写字母开头的 [a-z0-9_-]（2–64 位）")
		return
	}
	if existing, _ := repo.GetBizCalendarByName(name); existing != nil {
		redirectFlash(c, "/console/calendars", "err", "同名日历已存在：%s", name)
		return
	}
	if count := len(cals()); count >= maxCalendarCount {
		redirectFlash(c, "/console/calendars", "err", "日历数量已达上限（%d）", maxCalendarCount)
		return
	}
	cal := &model.BizCalendar{
		CalendarID:  uuid.NewString(),
		Name:        name,
		Description: strings.TrimSpace(c.PostForm("description")),
		CreatedBy:   s.Username,
	}
	if err := repo.CreateBizCalendar(cal); err != nil {
		redirectFlash(c, "/console/calendars", "err", "保存失败：%v", err)
		return
	}
	redirectFlash(c, "/console/calendars", "ok", "日历已创建：%s（可维护日期了）", name)
}

func (r *Router) handleCalendarDateAdd(c *gin.Context) {
	if _, ok := requireAdminPage(c); !ok {
		return
	}
	name := c.Param("name")
	cal, err := repo.GetBizCalendarByName(name)
	if err != nil || cal == nil {
		redirectFlash(c, "/console/calendars", "err", "日历不存在：%s", name)
		return
	}
	date, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(c.PostForm("date")), time.Local)
	if err != nil {
		redirectFlash(c, "/console/calendars?calendar="+name, "err", "日期格式应为 2006-01-02")
		return
	}
	dayType := strings.TrimSpace(c.PostForm("day_type"))
	if dayType != model.CalendarDayOff && dayType != model.CalendarDayWork {
		redirectFlash(c, "/console/calendars?calendar="+name, "err", "day_type 只能是 off / work")
		return
	}
	if err := repo.UpsertBizCalendarDate(cal.CalendarID, date, dayType); err != nil {
		redirectFlash(c, "/console/calendars?calendar="+name, "err", "保存失败：%v", err)
		return
	}
	redirectFlash(c, "/console/calendars?calendar="+name, "ok", "已设置 %s = %s", date.Format("2006-01-02"), dayType)
}

func (r *Router) handleCalendarDateDelete(c *gin.Context) {
	if _, ok := requireAdminPage(c); !ok {
		return
	}
	name := c.Param("name")
	date, err := time.ParseInLocation("2006-01-02", c.Param("date"), time.Local)
	if err != nil {
		redirectFlash(c, "/console/calendars?calendar="+name, "err", "日期格式错误")
		return
	}
	if ok, err := repo.DeleteBizCalendarDate(name, date); err != nil || !ok {
		redirectFlash(c, "/console/calendars?calendar="+name, "err", "删除失败或不存在")
		return
	}
	redirectFlash(c, "/console/calendars?calendar="+name, "ok", "已移除 %s", date.Format("2006-01-02"))
}

func (r *Router) handleCalendarDelete(c *gin.Context) {
	if _, ok := requireAdminPage(c); !ok {
		return
	}
	name := c.Param("name")
	ok, err := repo.DeleteBizCalendar(name)
	if err != nil || !ok {
		redirectFlash(c, "/console/calendars", "err", "删除失败或不存在：%s", name)
		return
	}
	redirectFlash(c, "/console/calendars", "ok", "日历已删除：%s（引用它的任务请改指向，否则其 cron 物化会失败）", name)
}

// ── /api/calendars (CLI, admin) ─────────────────────────────────────────

func (r *Router) apiListCalendars(c *gin.Context) {
	cals, err := repo.ListBizCalendars()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": err.Error()})
		return
	}
	type cal struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Dates       int64  `json:"dates"`
	}
	out := make([]cal, 0, len(cals))
	for _, ca := range cals {
		n, _ := repo.CountBizCalendarDates(ca.CalendarID)
		out = append(out, cal{Name: ca.Name, Description: ca.Description, Dates: n})
	}
	c.JSON(http.StatusOK, gin.H{"items": out, "total": len(out)})
}

func (r *Router) apiUpsertCalendarDate(c *gin.Context) {
	var req dto.UpsertCalendarDateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "invalid request: " + err.Error()})
		return
	}
	cal, err := repo.GetBizCalendarByName(strings.TrimSpace(req.Name))
	if err != nil || cal == nil {
		c.JSON(http.StatusNotFound, gin.H{"detail": "calendar not found"})
		return
	}
	date, perr := time.ParseInLocation("2006-01-02", strings.TrimSpace(req.Date), time.Local)
	if perr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "date must be 2006-01-02"})
		return
	}
	if req.DayType != model.CalendarDayOff && req.DayType != model.CalendarDayWork {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "day_type must be off | work"})
		return
	}
	if err := repo.UpsertBizCalendarDate(cal.CalendarID, date, req.DayType); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"name": req.Name, "date": req.Date, "day_type": req.DayType})
}

func (r *Router) apiCreateCalendar(c *gin.Context) {
	operator := ""
	if s, ok := auth.CurrentUser(c); ok {
		operator = s.Username
	}
	var req dto.CreateCalendarRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "invalid request: " + err.Error()})
		return
	}
	name := strings.TrimSpace(req.Name)
	if !calendarNameRe.MatchString(name) {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "name must match [a-z][a-z0-9_-]{1,63}"})
		return
	}
	if existing, _ := repo.GetBizCalendarByName(name); existing != nil {
		c.JSON(http.StatusConflict, gin.H{"detail": "calendar exists: " + name})
		return
	}
	cal := &model.BizCalendar{
		CalendarID:  uuid.NewString(),
		Name:        name,
		Description: strings.TrimSpace(req.Description),
		CreatedBy:   operator,
	}
	if err := repo.CreateBizCalendar(cal); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"name": name, "created": true})
}

func (r *Router) apiDeleteCalendar(c *gin.Context) {
	ok, err := repo.DeleteBizCalendar(c.Param("name"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": err.Error()})
		return
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"detail": "calendar not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

// cals is a tiny helper (used by the create guard).
func cals() []model.BizCalendar {
	out, _ := repo.ListBizCalendars()
	return out
}

// notifyChannelNames lists enabled notify channels (task-form alert dropdown).
func notifyChannelNames() []string {
	chs, err := repo.ListNotifyChannels()
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(chs))
	for _, ch := range chs {
		if ch.Enabled {
			names = append(names, ch.Name)
		}
	}
	return names
}
