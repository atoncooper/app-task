package service

// 业务日历与 cron 物化的桥接：解析 5 段 cron 的 dom/month/dow 字段，按日历
// 覆盖规则逐日判定可触发性。
//
// 生效规则（一句话）：被标 "work"（调休补班）的日期按周一参与星期匹配；
// 被标 "off"（节假日）的日期整日跳过；未标注的日期按 cron 原义。
// dom 与 dow 同时受限时沿用标准 cron 的 OR 语义（与 cronexpr 一致），
// 保证同一表达式"有无日历"的行为差异仅来自日历本身。

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"app-task/internal/model"
	"app-task/internal/repo"
)

const bizCalendarHorizonDays = 400 // 逐日推进上限：超窗报错而非静默丢触发

// cronFields holds the parsed sets of a 5-field cron expression (minute hour
// dom month dow). domAny/dowAny record "*" fields for the standard OR rule.
type cronFields struct {
	minute map[int]bool
	hour   map[int]bool
	dom    map[int]bool
	month  map[int]bool
	dow    map[int]bool // 0=Sunday, 7 also normalized to 0
	domAny bool
	dowAny bool
}

// parseCronFields parses a 5-field cron into matchable sets. Supports the
// syntax cronexpr accepts: "*" / "n" / "a-b" / lists / "*/step" / "a-b/step".
// Numeric only for dom/month (dow names are rarely used here and cronexpr
// remains the validator at registration time).
func parseCronFields(expr string) (*cronFields, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron %q: want 5 fields", expr)
	}
	minute, err := parseCronField(fields[0], 0, 59)
	if err != nil {
		return nil, fmt.Errorf("cron minute: %w", err)
	}
	hour, err := parseCronField(fields[1], 0, 23)
	if err != nil {
		return nil, fmt.Errorf("cron hour: %w", err)
	}
	dom, err := parseCronField(fields[2], 1, 31)
	if err != nil {
		return nil, fmt.Errorf("cron dom: %w", err)
	}
	month, err := parseCronField(fields[3], 1, 12)
	if err != nil {
		return nil, fmt.Errorf("cron month: %w", err)
	}
	dow, err := parseCronField(fields[4], 0, 7)
	if err != nil {
		return nil, fmt.Errorf("cron dow: %w", err)
	}
	if dow[7] { // 7 == Sunday
		dow[0] = true
		delete(dow, 7)
	}
	return &cronFields{
		minute: minute, hour: hour, dom: dom, month: month, dow: dow,
		domAny: fields[2] == "*", dowAny: fields[4] == "*",
	}, nil
}

// parseCronField expands one cron field into its value set.
func parseCronField(field string, min, max int) (map[int]bool, error) {
	out := map[int]bool{}
	for _, part := range strings.Split(field, ",") {
		step := 1
		rangePart := part
		if i := strings.Index(part, "/"); i >= 0 {
			rangePart = part[:i]
			v, perr := strconv.Atoi(part[i+1:])
			if perr != nil || v <= 0 {
				return nil, fmt.Errorf("bad step %q", part)
			}
			step = v
		}
		lo, hi := min, max
		switch {
		case rangePart == "*" || rangePart == "":
			// full range with optional step
		case strings.Contains(rangePart, "-"):
			bounds := strings.SplitN(rangePart, "-", 2)
			a, err1 := strconv.Atoi(bounds[0])
			b, err2 := strconv.Atoi(bounds[1])
			if err1 != nil || err2 != nil || a > b || a < min || b > max {
				return nil, fmt.Errorf("bad range %q", part)
			}
			lo, hi = a, b
		default:
			v, err := strconv.Atoi(rangePart)
			if err != nil || v < min || v > max {
				return nil, fmt.Errorf("bad value %q", part)
			}
			if step > 1 { // vixie 语义 "5/10" = 5,15,25,… 到上限
				lo, hi = v, max
			} else {
				lo, hi = v, v
			}
		}
		for v := lo; v <= hi; v += step {
			out[v] = true
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty field %q", field)
	}
	return out, nil
}

// dayMatches reports whether date d (with effective dow) fires under cf.
func (cf *cronFields) dayMatches(d time.Time, effDow int) bool {
	if !cf.month[int(d.Month())] {
		return false
	}
	var dayMatch bool
	switch {
	case cf.domAny && cf.dowAny:
		dayMatch = true
	case cf.domAny:
		dayMatch = cf.dow[effDow]
	case cf.dowAny:
		dayMatch = cf.dom[d.Day()]
	default:
		dayMatch = cf.dom[d.Day()] || cf.dow[effDow]
	}
	return dayMatch
}

// nextTriggerWithCalendar iterates day-by-day from `from` and returns the
// first schedulable trigger instant. Day iteration is capped at 400 days.
func nextTriggerWithCalendar(expr string, from time.Time, off, work map[string]bool) (time.Time, error) {
	cf, err := parseCronFields(expr)
	if err != nil {
		return time.Time{}, err
	}
	// earliest usable (hour, minute) combos, ascending
	type hm struct{ h, m int }
	var combos []hm
	for h := 0; h <= 23; h++ {
		if !cf.hour[h] {
			continue
		}
		for m := 0; m <= 59; m++ {
			if cf.minute[m] {
				combos = append(combos, hm{h, m})
			}
		}
	}

	day := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, from.Location())
	for i := 0; i < bizCalendarHorizonDays; i++ {
		key := day.Format("2006-01-02")
		if !off[key] {
			effDow := int(day.Weekday())
			if work[key] {
				effDow = 1 // 调休补班：按周一参与星期匹配
			}
			if cf.dayMatches(day, effDow) {
				for _, c := range combos {
					t := time.Date(day.Year(), day.Month(), day.Day(), c.h, c.m, 0, 0, from.Location())
					if t.After(from) {
						return t, nil
					}
				}
			}
		}
		day = day.AddDate(0, 0, 1)
	}
	return time.Time{}, fmt.Errorf("cron %q: no schedulable date within %d days (calendar excluded them all?)", expr, bizCalendarHorizonDays)
}

// NextTriggerForTask resolves a cron task's next occurrence, applying the
// task's business calendar when it references one. A missing/broken calendar
// fails loud — the cron chain pauses visibly instead of silently firing on
// holidays.
func NextTriggerForTask(expr string, from time.Time, calendarID string) (time.Time, error) {
	if calendarID == "" {
		return NextCronTrigger(expr, from)
	}
	// 任务引用的是日历名称（biz_calendar.name 唯一键），先解析成 uuid 主键。
	cal, err := repo.GetBizCalendarByName(calendarID)
	if err != nil {
		return time.Time{}, fmt.Errorf("load business calendar %q: %w", calendarID, err)
	}
	if cal == nil {
		return time.Time{}, fmt.Errorf("business calendar %q not found (deleted? re-point the task)", calendarID)
	}
	dates, err := repo.ListBizCalendarDates(cal.CalendarID)
	if err != nil {
		return time.Time{}, fmt.Errorf("load business calendar %q: %w", calendarID, err)
	}
	off := map[string]bool{}
	work := map[string]bool{}
	for _, d := range dates {
		key := d.Date.Format("2006-01-02")
		switch d.DayType {
		case model.CalendarDayOff:
			off[key] = true
		case model.CalendarDayWork:
			work[key] = true
		}
	}
	return nextTriggerWithCalendar(expr, from, off, work)
}
