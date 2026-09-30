package service

// 业务日历专项测试：cron 字段解析、生效规则（节假日跳过 / 调休补班按周一
// 参与星期匹配）、dom/dow OR 语义与 cronexpr 的一致性，以及物化端到端。

import (
	"testing"
	"time"

	"app-task/internal/model"
	"app-task/internal/repo"
)

// mustDate builds a time at 00:00 local.
func mustDate(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

func TestParseCronFields(t *testing.T) {
	cf, err := parseCronFields("*/15 9-17 1,15 * 1-5")
	if err != nil {
		t.Fatal(err)
	}
	if !cf.minute[0] || !cf.minute[15] || cf.minute[1] {
		t.Fatalf("minute set wrong: %v", cf.minute)
	}
	if !cf.hour[9] || !cf.hour[17] || cf.hour[8] {
		t.Fatalf("hour set wrong")
	}
	if !cf.dom[1] || !cf.dom[15] || cf.domAny {
		t.Fatalf("dom set wrong")
	}
	if !cf.dow[1] || !cf.dow[5] || cf.dow[6] || cf.dow[7] {
		t.Fatalf("dow set wrong (7 must normalize to 0)")
	}
	if _, err := parseCronFields("0 9 * *"); err == nil {
		t.Fatal("4-field cron must error")
	}
}

func TestNextTriggerWithCalendarHolidaySkip(t *testing.T) {
	// 每天 09:00；明天是法定节假日 → 跳到后天。
	// from 固定为今天中午：保证"今天 09:00"已过，消除运行时刻抖动。
	now := time.Date(time.Now().Local().Year(), time.Now().Local().Month(), time.Now().Local().Day(), 12, 0, 0, 0, time.Local)
	tomorrow := now.AddDate(0, 0, 1)
	dayAfter := now.AddDate(0, 0, 2)
	off := map[string]bool{tomorrow.Format("2006-01-02"): true}

	got, err := nextTriggerWithCalendar("0 9 * * *", now, off, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(dayAfter.Year(), dayAfter.Month(), dayAfter.Day(), 9, 0, 0, 0, now.Location())
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v (must skip holiday)", got, want)
	}
	// 无日历 → 明天 09:00（行为不变基线）
	plain, err := nextTriggerWithCalendar("0 9 * * *", now, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantPlain := time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 9, 0, 0, 0, now.Location())
	if !plain.Equal(wantPlain) {
		t.Fatalf("plain = %v, want %v", plain, wantPlain)
	}
}

func TestNextTriggerWithCalendarWorkdayOverride(t *testing.T) {
	// 找下一个周六：调休标 work。1-5 的 cron 应在该周六 09:00 触发；
	// 周六专属的 cron（dow=6）不应触发（调休日按周一匹配）。
	// 起点固定为"下一个周五的中午"：周五 09:00 已过，"1-5" 的下一个候选
	// 只能是紧随其后的周六（被标 work 后按周一匹配触发）。
	fri := time.Now().Local()
	for fri.Weekday() != time.Friday {
		fri = fri.AddDate(0, 0, 1)
	}
	fri = time.Date(fri.Year(), fri.Month(), fri.Day(), 12, 0, 0, 0, time.Local)
	nextSat := fri.AddDate(0, 0, 1)
	satKey := nextSat.Format("2006-01-02")

	work := map[string]bool{satKey: true}
	got, err := nextTriggerWithCalendar("0 9 * * 1-5", fri, nil, work)
	if err != nil {
		t.Fatal(err)
	}
	if got.Format("2006-01-02") != satKey {
		t.Fatalf("weekday cron must fire on adjusted workday Saturday: got %v", got)
	}
	if got.Weekday() != time.Saturday || got.Hour() != 9 {
		t.Fatalf("got %v, want Saturday 09:00", got)
	}

	// 周六专属 cron：调休日按周一匹配 → 不应在周六触发
	got2, err := nextTriggerWithCalendar("0 9 * * 6", fri, nil, work)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Format("2006-01-02") == satKey {
		t.Fatalf("Saturday-only cron must not fire on adjusted workday Saturday: %v", got2)
	}
}

func TestNextTriggerDomDowOrSemantics(t *testing.T) {
	// 标准 cron：dom 与 dow 同时受限 → OR。10 号且是周三 → 三天内必有一次。
	now := time.Now().Local()
	off := map[string]bool{}
	got, err := nextTriggerWithCalendar("0 9 10 * 3", now, off, nil)
	if err != nil {
		t.Fatal(err)
	}
	ok := got.Day() == 10 || got.Weekday() == time.Wednesday
	if !ok {
		t.Fatalf("dom/dow OR semantics broken: %v", got)
	}
}

func TestNextTriggerHorizonExhausted(t *testing.T) {
	// 2 月 30 日不存在 → 400 天内无解必须报错而非静默。
	if _, err := nextTriggerWithCalendar("0 9 30 2 *", time.Now().Local(), nil, nil); err == nil {
		t.Fatal("unsatisfiable cron must error")
	}
}

func TestRegisterTaskWithCalendarSkipsHoliday(t *testing.T) {
	setupTestDB(t)
	svc := NewTaskService()
	cal := &model.BizCalendar{CalendarID: "cal-1", Name: "cn-test", CreatedBy: "itest"}
	if err := repo.CreateBizCalendar(cal); err != nil {
		t.Fatal(err)
	}
	tomorrow := time.Now().Local().AddDate(0, 0, 1)
	if err := repo.UpsertBizCalendarDate("cal-1", mustDate(tomorrow.Format("2006-01-02")), model.CalendarDayOff); err != nil {
		t.Fatal(err)
	}

	taskID, err := svc.RegisterTask(RegisterOptions{
		TaskType: "http", ExecutorURL: "http://x",
		CronExpr: "0 9 * * *", CalendarID: "cn-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, _ := repo.GetTaskByID(taskID)
	if task.CalendarID != "cn-test" {
		t.Fatalf("calendar_id = %q", task.CalendarID)
	}
	if task.TriggerTime.Format("2006-01-02") == tomorrow.Format("2006-01-02") {
		t.Fatalf("trigger landed on holiday: %v", task.TriggerTime)
	}
	// 触发时刻的墙钟必须是 09:00（业务时区）
	if task.TriggerTime.In(time.Local).Hour() != 9 || task.TriggerTime.In(time.Local).Minute() != 0 {
		t.Fatalf("trigger time-of-day wrong: %v", task.TriggerTime.In(time.Local))
	}
}

func TestExtendCronUsesCalendar(t *testing.T) {
	setupTestDB(t)
	svc := NewTaskService()
	cal := &model.BizCalendar{CalendarID: "cal-2", Name: "cn-ext", CreatedBy: "itest"}
	if err := repo.CreateBizCalendar(cal); err != nil {
		t.Fatal(err)
	}
	// 明天标休
	tomorrow := time.Now().Local().AddDate(0, 0, 1)
	if err := repo.UpsertBizCalendarDate("cal-2", mustDate(tomorrow.Format("2006-01-02")), model.CalendarDayOff); err != nil {
		t.Fatal(err)
	}
	taskID, err := svc.RegisterTask(RegisterOptions{
		TaskType: "http", ExecutorURL: "http://x",
		CronExpr: "0 9 * * *", CalendarID: "cn-ext",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 完成当前实例 → tick 扩展下一次：扩展结果也必须跳过节假日
	repo.ConditionalUpdate(taskID, "pending", "completed", nil)
	newSched(t).tick()
	next, _ := repo.GetTaskByID(taskID)
	if next.CronNextTaskID == "" {
		t.Fatal("cron extension did not run")
	}
	nextTask, _ := repo.GetTaskByID(next.CronNextTaskID)
	if nextTask.TriggerTime.Format("2006-01-02") == tomorrow.Format("2006-01-02") {
		t.Fatalf("extension landed on holiday: %v", nextTask.TriggerTime)
	}
	if nextTask.CalendarID != "cn-ext" {
		t.Fatalf("clone lost calendar reference: %q", nextTask.CalendarID)
	}
}
