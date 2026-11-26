package config

import "testing"

func TestLoadDefaultsAndOverrides(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "127.0.0.1:9000" || cfg.MaxObjectSize != 1<<30 || cfg.ListPageSize != 1000 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}

	t.Setenv("GOSTORE_MAX_OBJECT_SIZE", "4096")
	t.Setenv("GOSTORE_LIST_PAGE_SIZE", "25")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxObjectSize != 4096 || cfg.ListPageSize != 25 {
		t.Fatalf("unexpected overrides: %+v", cfg)
	}

	t.Setenv("GOSTORE_MAX_OBJECT_SIZE", "0")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for zero max object size")
	}
	t.Setenv("GOSTORE_MAX_OBJECT_SIZE", "4096")
	t.Setenv("GOSTORE_LIST_PAGE_SIZE", "1001")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for list page size > 1000")
	}
}
