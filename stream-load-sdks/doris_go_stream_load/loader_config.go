package dorisstreamload

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

type LoaderConfig struct {
	Endpoint      string      `json:"endpoint,omitempty"`
	Database      string      `json:"database,omitempty"`
	Table         string      `json:"table,omitempty"`
	StreamLoadURL string      `json:"stream_load_url,omitempty"`
	Columns       []string    `json:"columns,omitempty"`
	Headers       http.Header `json:"headers,omitempty"`
	Mode          Mode        `json:"mode,omitempty"`

	AuthenticationType  AuthenticationType `json:"authentication_type,omitempty"`
	AuthenticationToken string             `json:"authentication_token,omitempty"`

	MaxQueueSize       int            `json:"max_queue_size,omitempty"`
	MaxUploadQueueSize int            `json:"max_upload_queue_size,omitempty"`
	BatchBytes         int            `json:"batch_bytes,omitempty"`
	Linger             string         `json:"linger,omitempty"`
	MaxQueueWaitTime   string         `json:"max_queue_wait_time,omitempty"`
	DorisUploadWorkers int            `json:"doris_upload_workers,omitempty"`
	Validation         ValidationMode `json:"validation,omitempty"`

	DorisUploadTimeout        string `json:"doris_upload_timeout,omitempty"`
	DorisUploadRequestTimeout string `json:"doris_upload_request_timeout,omitempty"`
	CallbackTimeout           string `json:"callback_timeout,omitempty"`
	SlowCallbackWarn          string `json:"slow_callback_warn,omitempty"`
	LabelPrefix               string `json:"label_prefix,omitempty"`
	StatusPollTimeout         string `json:"status_poll_timeout,omitempty"`
	FakeSend                  bool   `json:"fake_send,omitempty"`
	FakeSendDelay             string `json:"fake_send_delay,omitempty"`
	FakeSendDelaySet          bool   `json:"fake_send_delay_set,omitempty"`

	CSVSeparator string `json:"csv_separator,omitempty"`
	CSVQuote     string `json:"csv_quote,omitempty"`

	TLSSkipVerify bool   `json:"tls_skip_verify,omitempty"`
	TLSCACertPath string `json:"tls_ca_cert_path,omitempty"`

	LogLevel    LogLevel `json:"log_level,omitempty"`
	LogLevelSet bool     `json:"log_level_set,omitempty"`
}

func LoadLoaderConfig(path string) (LoaderConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return LoaderConfig{}, err
	}
	var cfg LoaderConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return LoaderConfig{}, err
	}
	return cfg, nil
}

func SaveLoaderConfig(path string, cfg LoaderConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func LoaderConfigFromConfig(cfg Config) LoaderConfig {
	return LoaderConfig{
		Endpoint:                  cfg.Endpoint,
		Database:                  cfg.Database,
		Table:                     cfg.Table,
		StreamLoadURL:             cfg.StreamLoadURL,
		Columns:                   append([]string(nil), cfg.Columns...),
		Headers:                   cfg.Headers.Clone(),
		Mode:                      cfg.Mode,
		AuthenticationType:        cfg.AuthenticationType,
		AuthenticationToken:       cfg.AuthenticationToken,
		MaxQueueSize:              cfg.MaxQueueSize,
		MaxUploadQueueSize:        cfg.MaxUploadQueueSize,
		BatchBytes:                cfg.BatchBytes,
		Linger:                    formatDuration(cfg.Linger),
		MaxQueueWaitTime:          formatDuration(cfg.MaxQueueWaitTime),
		DorisUploadWorkers:        cfg.DorisUploadWorkers,
		Validation:                cfg.Validation,
		DorisUploadTimeout:        formatDuration(cfg.DorisUploadTimeout),
		DorisUploadRequestTimeout: formatDuration(cfg.DorisUploadRequestTimeout),
		CallbackTimeout:           formatDuration(cfg.CallbackTimeout),
		SlowCallbackWarn:          formatDuration(cfg.SlowCallbackWarn),
		LabelPrefix:               cfg.LabelPrefix,
		StatusPollTimeout:         formatDuration(cfg.StatusPollTimeout),
		FakeSend:                  cfg.FakeSend,
		FakeSendDelay:             formatDuration(cfg.FakeSendDelay),
		FakeSendDelaySet:          cfg.FakeSendDelaySet,
		CSVSeparator:              cfg.CSVSeparator,
		CSVQuote:                  cfg.CSVQuote,
		TLSSkipVerify:             cfg.TLSSkipVerify,
		TLSCACertPath:             cfg.TLSCACertPath,
		LogLevel:                  cfg.LogLevel,
		LogLevelSet:               cfg.LogLevelSet || cfg.LogLevel != 0,
	}
}

func (lc LoaderConfig) Config() (Config, error) {
	cfg := Config{
		Endpoint:            lc.Endpoint,
		Database:            lc.Database,
		Table:               lc.Table,
		StreamLoadURL:       lc.StreamLoadURL,
		Columns:             append([]string(nil), lc.Columns...),
		Headers:             lc.Headers.Clone(),
		Mode:                lc.Mode,
		AuthenticationType:  lc.AuthenticationType,
		AuthenticationToken: lc.AuthenticationToken,
		MaxQueueSize:        lc.MaxQueueSize,
		MaxUploadQueueSize:  lc.MaxUploadQueueSize,
		BatchBytes:          lc.BatchBytes,
		DorisUploadWorkers:  lc.DorisUploadWorkers,
		Validation:          lc.Validation,
		LabelPrefix:         lc.LabelPrefix,
		CSVSeparator:        lc.CSVSeparator,
		CSVQuote:            lc.CSVQuote,
		TLSSkipVerify:       lc.TLSSkipVerify,
		TLSCACertPath:       lc.TLSCACertPath,
		LogLevel:            lc.LogLevel,
		LogLevelSet:         lc.LogLevelSet || lc.LogLevel != 0,
		FakeSend:            lc.FakeSend,
		FakeSendDelaySet:    lc.FakeSendDelay != "" || lc.FakeSendDelaySet,
	}

	var err error
	if cfg.Linger, err = parseOptionalDuration("linger", lc.Linger); err != nil {
		return Config{}, err
	}
	if cfg.MaxQueueWaitTime, err = parseOptionalDuration("max_queue_wait_time", lc.MaxQueueWaitTime); err != nil {
		return Config{}, err
	}
	if cfg.DorisUploadTimeout, err = parseOptionalDuration("doris_upload_timeout", lc.DorisUploadTimeout); err != nil {
		return Config{}, err
	}
	if cfg.DorisUploadRequestTimeout, err = parseOptionalDuration("doris_upload_request_timeout", lc.DorisUploadRequestTimeout); err != nil {
		return Config{}, err
	}
	if cfg.CallbackTimeout, err = parseOptionalDuration("callback_timeout", lc.CallbackTimeout); err != nil {
		return Config{}, err
	}
	if cfg.SlowCallbackWarn, err = parseOptionalDuration("slow_callback_warn", lc.SlowCallbackWarn); err != nil {
		return Config{}, err
	}
	if cfg.StatusPollTimeout, err = parseOptionalDuration("status_poll_timeout", lc.StatusPollTimeout); err != nil {
		return Config{}, err
	}
	if cfg.FakeSendDelay, err = parseOptionalDuration("fake_send_delay", lc.FakeSendDelay); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func parseOptionalDuration(field, value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	return duration, nil
}

func formatDuration(duration time.Duration) string {
	if duration == 0 {
		return ""
	}
	return duration.String()
}
