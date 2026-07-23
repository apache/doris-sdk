package dorisstreamload

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestLoaderConfigConvertsToRuntimeConfig(t *testing.T) {
	loader := LoaderConfig{
		StreamLoadURL:             "http://example.invalid/api/db/t/_stream_load",
		Columns:                   []string{"c1", "c2"},
		Mode:                      ModeCSV,
		AuthenticationType:        AuthenticationBasic,
		AuthenticationToken:       "user:pass",
		Linger:                    "250ms",
		MaxQueueWaitTime:          "2s",
		DorisUploadTimeout:        "100s",
		DorisUploadRequestTimeout: "30s",
		FakeSend:                  true,
		FakeSendDelay:             "500ms",
		Validation:                ValidateSyntax,
		TLSSkipVerify:             true,
		TLSCACertPath:             "/tmp/test-ca.pem",
		LogLevel:                  LogLevelError,
		LogLevelSet:               true,
	}

	cfg, err := loader.Config()
	if err != nil {
		t.Fatalf("Config() error = %v", err)
	}
	if cfg.MaxQueueWaitTime != 2*time.Second {
		t.Fatalf("MaxQueueWaitTime = %s, want 2s", cfg.MaxQueueWaitTime)
	}
	if cfg.Linger != 250*time.Millisecond {
		t.Fatalf("Linger = %s, want 250ms", cfg.Linger)
	}
	if cfg.DorisUploadTimeout != 100*time.Second {
		t.Fatalf("DorisUploadTimeout = %s, want 100s", cfg.DorisUploadTimeout)
	}
	if cfg.DorisUploadRequestTimeout != 30*time.Second {
		t.Fatalf("DorisUploadRequestTimeout = %s, want 30s", cfg.DorisUploadRequestTimeout)
	}
	if !cfg.FakeSend {
		t.Fatal("FakeSend = false, want true")
	}
	if cfg.FakeSendDelay != 500*time.Millisecond {
		t.Fatalf("FakeSendDelay = %s, want 500ms", cfg.FakeSendDelay)
	}
	if cfg.Validation != ValidateSyntax {
		t.Fatalf("Validation = %q, want %q", cfg.Validation, ValidateSyntax)
	}
	if !cfg.TLSSkipVerify {
		t.Fatal("TLSSkipVerify = false, want true")
	}
	if cfg.TLSCACertPath != "/tmp/test-ca.pem" {
		t.Fatalf("TLSCACertPath = %q, want /tmp/test-ca.pem", cfg.TLSCACertPath)
	}
	if !cfg.FakeSendDelaySet {
		t.Fatal("FakeSendDelaySet = false, want true")
	}
	if cfg.LogLevel != LogLevelError || !cfg.LogLevelSet {
		t.Fatalf("log level = %d set=%t, want error set=true", cfg.LogLevel, cfg.LogLevelSet)
	}
	if !reflect.DeepEqual(cfg.Columns, []string{"c1", "c2"}) {
		t.Fatalf("Columns = %#v", cfg.Columns)
	}
}

func TestLoaderConfigSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loader.json")
	want := LoaderConfig{
		Endpoint:         "http://example.invalid/api/db/table/_stream_load",
		Database:         "db",
		Table:            "tbl",
		Columns:          []string{"c1"},
		Mode:             ModeJSON,
		MaxQueueWaitTime: "1s",
		Validation:       ValidateSyntax,
	}

	if err := SaveLoaderConfig(path, want); err != nil {
		t.Fatalf("SaveLoaderConfig() error = %v", err)
	}
	got, err := LoadLoaderConfig(path)
	if err != nil {
		t.Fatalf("LoadLoaderConfig() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loaded config = %#v, want %#v", got, want)
	}
}

func TestLoaderConfigRejectsInvalidDuration(t *testing.T) {
	_, err := (LoaderConfig{MaxQueueWaitTime: "soon"}).Config()
	if err == nil {
		t.Fatal("Config() error = nil, want invalid duration error")
	}
}
