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
		cfg.MaxMultipartSize != 5<<30 || cfg.MinPartSize != 5<<20 || cfg.MaxUploadParts != 10000 ||
		cfg.MaxConcurrentRequests != 256 || cfg.MinFreeSpace != 1<<30 ||
		cfg.AccessKey != "test-access" || cfg.SecretKey != "test-secret" || cfg.Region != "us-east-1" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}

	t.Setenv("GOSTORE_MAX_OBJECT_SIZE", "4096")
	t.Setenv("GOSTORE_MAX_MULTIPART_SIZE", "8192")
	t.Setenv("GOSTORE_MIN_PART_SIZE", "512")
	t.Setenv("GOSTORE_MAX_UPLOAD_PARTS", "20")
	t.Setenv("GOSTORE_UPLOAD_EXPIRY", "10m")
	t.Setenv("GOSTORE_MAX_CONCURRENT_REQUESTS", "32")
	t.Setenv("GOSTORE_IDLE_TRANSFER_TIMEOUT", "15s")
	t.Setenv("GOSTORE_MIN_FREE_SPACE", "2048")
	t.Setenv("GOSTORE_LIST_PAGE_SIZE", "25")
	t.Setenv("GOSTORE_REGION", "eu-west-1")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxObjectSize != 4096 || cfg.MaxMultipartSize != 8192 || cfg.MinPartSize != 512 ||
		cfg.MaxUploadParts != 20 || cfg.MaxConcurrentRequests != 32 || cfg.MinFreeSpace != 2048 ||
		cfg.ListPageSize != 25 || cfg.Region != "eu-west-1" {
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
