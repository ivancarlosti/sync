package database

import (
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestMigrateWidensTheReportSectionRowsColumn pins the upgrade path of an instance
// that is already running: the fix of "Error 1406 (22001): Data too long for column
// 'rows'" has to reach the databases that were created before it, not only new
// installs.
//
// Migration runs on every boot (see docs/database.md), and GORM only alters a
// column whose declared type is not already the stored one — "text" is not a prefix
// of "longtext", and the only type aliases the MySQL driver declares are bool and
// tinyint — so the rows of every existing report_sections table are widened in
// place. The DDL of the recreated table says whether that happened, and the rows
// stored before the upgrade have to survive it.
func TestMigrateWidensTheReportSectionRowsColumn(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "legacy.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	// The schema of a database created before the column was widened, with one
	// report section already stored in it.
	legacy := "CREATE TABLE `report_sections` (`id` integer PRIMARY KEY AUTOINCREMENT," +
		"`report_id` integer,`section` text,`columns` text,`rows` text,`row_count` integer," +
		"`truncated` numeric,`error` text,`created_at` datetime)"
	if err := db.Exec(legacy).Error; err != nil {
		t.Fatalf("creating the legacy table: %v", err)
	}
	if err := db.Exec("INSERT INTO `report_sections` (`report_id`, `section`, `columns`, `rows`, `row_count`) " +
		"VALUES (1, 'users', '[]', '[ [\"Ada\"] ]', 1)").Error; err != nil {
		t.Fatalf("seeding the legacy row: %v", err)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate = %v", err)
	}
	var ddl string
	if err := db.Raw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'report_sections'").Scan(&ddl).Error; err != nil {
		t.Fatalf("reading the table definition: %v", err)
	}
	if !strings.Contains(ddl, "`rows` longtext") {
		t.Fatalf("table definition = %s, want the rows column widened to longtext", ddl)
	}

	var rows string
	if err := db.Raw("SELECT `rows` FROM `report_sections` WHERE `id` = 1").Scan(&rows).Error; err != nil {
		t.Fatalf("reading the migrated row: %v", err)
	}
	if rows != `[ ["Ada"] ]` {
		t.Errorf("rows after the migration = %q, want the stored document untouched", rows)
	}

	// Migration runs on every boot, so a second pass over an already migrated
	// database has to be a no-op (and must not try to reshape the column again).
	if err := Migrate(db); err != nil {
		t.Fatalf("second Migrate = %v", err)
	}
}
