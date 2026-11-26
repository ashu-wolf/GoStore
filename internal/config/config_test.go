package config

import "testing"

func TestLoadDefaultsAndOverrides(t *testing.T) {
	if _, err := Load(); err == nil {
		t.Fatal("expected error when credentials are missing")
	}
	t.Setenv("GOSTORE_ACCESS_KEY", "test-access")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when secret key is missing")
	}
	t.Setenv("GOSTORE_SECRET_KEY", "test-secret")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "127.0.0.1:9000" || cfg.MaxObjectSize != 1<<30 || cfg.ListPageSize != 1000 ||
		cfg.AccessKey != "test-access" || cfg.SecretKey != "test-secret" || cfg.Region != "us-east-1" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}

	t.Setenv("GOSTORE_MAX_OBJECT_SIZE", "4096")
	t.Setenv("GOSTORE_LIST_PAGE_SIZE", "25")
	t.Setenv("GOSTORE_REGION", "eu-west-1")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxObjectSize != 4096 || cfg.ListPageSize != 25 || cfg.Region != "eu-west-1" {
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
