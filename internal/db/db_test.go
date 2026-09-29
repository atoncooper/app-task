package db

// Tests for DSN building: sqlalchemy-style conversion, param appending, and
// the config-provided timeouts/TLS. Plus migration on a fresh database.

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestToGormDSN(t *testing.T) {
	params := map[string]string{
		"charset":   "utf8mb4",
		"parseTime": "True",
		"loc":       "Local",
		"timeout":   "10s",
		"tls":       "skip-verify",
	}
	got, err := toGormDSN("mysql+aiomysql://u:p@app-task-mysql:3306/app_task?old=1", params)
	if err != nil {
		t.Fatal(err)
	}
	// params render sorted by key (deterministic)
	want := "u:p@tcp(app-task-mysql:3306)/app_task?charset=utf8mb4&loc=Local&parseTime=True&timeout=10s&tls=skip-verify"
	if got != want {
		t.Fatalf("dsn = %q, want %q", got, want)
	}

	// no-user / no-db edge cases still build
	if got, err = toGormDSN("mysql://app-task-mysql:3306", params); err != nil || got == "" {
		t.Fatalf("no-user dsn = %q %v", got, err)
	}
	if _, err := toGormDSN("mysql:///app", params); err == nil {
		t.Fatal("missing host must error")
	}
}

func TestDSNParams(t *testing.T) {
	p := dsnParams(Options{DialTimeoutSec: 10, ReadTimeoutSec: 60, TLS: "true"})
	if p["timeout"] != "10s" || p["readTimeout"] != "60s" || p["tls"] != "true" {
		t.Fatalf("params = %v", p)
	}
	if _, has := p["writeTimeout"]; has {
		t.Fatal("zero value must be omitted")
	}
	// base params always present
	if p["charset"] != "utf8mb4" || p["parseTime"] != "True" || p["loc"] != "Local" {
		t.Fatalf("base params = %v", p)
	}
}

// TestMigrateFreshDatabase guards the fresh-install path: Migrate must create
// the full schema on an empty database. It used to abort on the api_key dedupe
// pre-check, which queried the table before AutoMigrate created it
// (Error 1146 on MySQL, "no such table" on SQLite) — a crash loop on every
// first boot.
func TestMigrateFreshDatabase(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatalf("get *sql.DB: %v", err)
	}
	defer sqlDB.Close()

	prev := DB
	DB = gdb
	defer func() { DB = prev }()

	if err := Migrate(); err != nil {
		t.Fatalf("migrate on fresh database: %v", err)
	}
	for _, table := range []string{"api_key", "task", "task_log", "webui_user", "cluster_node"} {
		if !DB.Migrator().HasTable(table) {
			t.Fatalf("table %s not created", table)
		}
	}

	// A second run must stay idempotent (dedupe runs against the now-existing
	// table and finds no duplicates).
	if err := Migrate(); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
}
