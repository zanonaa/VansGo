package database

import (
	"context"
	"testing"
	"time"
)

func TestRecordDetailCapsAt1000Rows(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir(), "NORMAL")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = Migrate(ctx, db, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	u := Usage{DB: db}
	for i := 0; i < 1005; i++ {
		detail := map[string]any{"id": time.Now().Format("20060102150405.000000000"), "timestamp": time.UnixMilli(int64(1700000000000 + i)).UTC().Format("2006-01-02T15:04:05.000Z"), "status": "success"}
		if err = u.RecordDetail(ctx, detail); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM requestDetails`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1000 {
		t.Fatalf("rows = %d, want 1000", count)
	}
	// As linhas antigas (timestamps menores) é que saem.
	var oldest int
	if err = db.QueryRow(`SELECT count(*) FROM requestDetails WHERE timestamp < ?`, time.UnixMilli(1700000000000+5).UTC().Format("2006-01-02T15:04:05.000Z")).Scan(&oldest); err != nil {
		t.Fatal(err)
	}
	if oldest != 0 {
		t.Fatalf("old rows survived: %d", oldest)
	}
}
