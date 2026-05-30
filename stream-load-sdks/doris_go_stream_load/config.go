package dorisstreamload

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/pkcs12"
)

const (
	maxBatchBytes                    = 90 * 1024 * 1024
	defaultMaxQueueSize              = 100_000
	defaultMaxUploadQueueSize        = 1
	defaultBatchBytes                = maxBatchBytes
	defaultDorisUploadWorkers        = 1
	defaultLinger                    = 5 * time.Millisecond
	defaultDorisUploadTimeout        = 300 * time.Second
	defaultDorisUploadRequestTimeout = 300 * time.Second
	minDorisUploadRequestTimeout     = 10 * time.Second
	defaultCallbackTimeout           = 100 * time.Millisecond
	defaultSlowCallback              = 10 * time.Millisecond
	defaultFakeSendDelay             = 500 * time.Millisecond
	defaultCSVSeparator              = ","
	defaultCSVQuote                  = `"`
	defaultLogLevel                  = LogLevelInfo
	defaultLabelPrefix               = "go_stream_load"
	defaultStatusPollTimeout         = 300 * time.Second
)

var (
	statusPollInitialBackoff  = 500 * time.Millisecond
	statusPollMaxBackoff      = 4 * time.Second
	uploadRetryInitialBackoff = 1 * time.Second
	uploadRetryMaxBackoff     = 4 * time.Second
)

type Mode string
type ValidationMode string
type AuthenticationType string

const (
	ModeCSV  Mode = "csv"
	ModeJSON Mode = "json"

	ValidateNone   ValidationMode = "none"
	ValidateSyntax ValidationMode = "syntax"
	ValidateStrict ValidationMode = "strict"

	AuthenticationNone  AuthenticationType = ""
	AuthenticationBasic AuthenticationType = "basic"
)

type LogLevel int

const (
	LogLevelError LogLevel = iota
	LogLevelInfo
	LogLevelDebug
)

type Logger interface {
	Printf(format string, args ...any)
}

type Config struct {
	Endpoint      string
	Database      string
	Table         string
	StreamLoadURL string

	Columns []string
	Headers http.Header
	Mode    Mode

	AuthenticationType  AuthenticationType
	AuthenticationToken string

	MaxQueueSize       int
	MaxUploadQueueSize int
	BatchBytes         int
	Linger             time.Duration
	MaxQueueWaitTime   time.Duration
	DorisUploadWorkers int
	Validation         ValidationMode

	DorisUploadTimeout        time.Duration
	DorisUploadRequestTimeout time.Duration
	CallbackTimeout           time.Duration
	SlowCallbackWarn          time.Duration
	LabelPrefix               string
	StatusPollTimeout         time.Duration
	FakeSend                  bool
	FakeSendDelay             time.Duration
	FakeSendDelaySet          bool

	CSVSeparator string
	CSVQuote     string

	TLSSkipVerify bool
	TLSCACertPath string

	Logger   Logger
	LogLevel LogLevel
	// LogLevelSet distinguishes an explicit LogLevelError from the zero-value
	// default path. Non-zero log levels are treated as explicitly configured.
	LogLevelSet bool

	HTTPClient *http.Client
}

func (c Config) withDefaults() Config {
	if c.MaxQueueSize <= 0 {
		c.MaxQueueSize = defaultMaxQueueSize
	}
	if c.MaxUploadQueueSize <= 0 {
		c.MaxUploadQueueSize = defaultMaxUploadQueueSize
	}
	if c.BatchBytes == 0 {
		c.BatchBytes = defaultBatchBytes
	}
	if c.DorisUploadWorkers <= 0 {
		c.DorisUploadWorkers = defaultDorisUploadWorkers
	}
	if c.Linger <= 0 {
		c.Linger = defaultLinger
	}
	if c.DorisUploadTimeout == 0 {
		c.DorisUploadTimeout = defaultDorisUploadTimeout
	}
	if c.DorisUploadRequestTimeout == 0 {
		c.DorisUploadRequestTimeout = defaultDorisUploadRequestTimeout
	}
	if c.CallbackTimeout <= 0 {
		c.CallbackTimeout = defaultCallbackTimeout
	}
	if c.SlowCallbackWarn <= 0 {
		c.SlowCallbackWarn = defaultSlowCallback
	}
	if !c.FakeSendDelaySet {
		c.FakeSendDelay = defaultFakeSendDelay
	}
	if c.LabelPrefix == "" {
		c.LabelPrefix = defaultLabelPrefix
	}
	if c.StatusPollTimeout <= 0 {
		c.StatusPollTimeout = defaultStatusPollTimeout
	}
	if c.CSVSeparator == "" {
		c.CSVSeparator = defaultCSVSeparator
	}
	if c.CSVQuote == "" {
		c.CSVQuote = defaultCSVQuote
	}
	if c.Mode == "" {
		c.Mode = ModeCSV
	}
	if c.Validation == "" {
		c.Validation = ValidateSyntax
	}
	if c.AuthenticationType == "" {
		c.AuthenticationType = AuthenticationNone
	}
	if !c.LogLevelSet && c.LogLevel == 0 {
		c.LogLevel = defaultLogLevel
	}
	if c.Headers == nil {
		c.Headers = make(http.Header)
	}
	return c
}

func (c Config) validate() error {
	if len(c.Columns) == 0 {
		return errors.New("columns must be configured")
	}
	switch c.Mode {
	case ModeCSV, ModeJSON:
	default:
		return errors.New("invalid mode")
	}
	switch c.Validation {
	case ValidateNone, ValidateSyntax, ValidateStrict:
	default:
		return errors.New("invalid validation mode")
	}
	switch c.AuthenticationType {
	case AuthenticationNone, AuthenticationBasic:
	default:
		return errors.New("invalid authentication type")
	}
	if c.AuthenticationType == AuthenticationBasic {
		if strings.TrimSpace(c.AuthenticationToken) == "" {
			return errors.New("authentication token is required for basic authentication")
		}
		if _, _, ok := strings.Cut(c.AuthenticationToken, ":"); !ok {
			return errors.New("basic authentication token must use user:password format")
		}
	}
	if c.StreamLoadURL == "" {
		if strings.TrimSpace(c.Endpoint) == "" {
			return errors.New("endpoint is required when stream load url is not set")
		}
		if err := validateEndpointURL(c.Endpoint); err != nil {
			return err
		}
		if strings.TrimSpace(c.Database) == "" {
			return errors.New("database is required when stream load url is not set")
		}
		if strings.TrimSpace(c.Table) == "" {
			return errors.New("table is required when stream load url is not set")
		}
	} else {
		if err := validateStreamLoadURL(c.StreamLoadURL); err != nil {
			return err
		}
	}
	if c.BatchBytes < 0 {
		return errors.New("batch bytes cannot be negative")
	}
	if c.BatchBytes > maxBatchBytes {
		return errors.New("batch bytes cannot be greater than 90 MiB")
	}
	if c.MaxQueueSize <= 0 {
		return errors.New("max queue size must be greater than zero")
	}
	if c.MaxQueueWaitTime < 0 {
		return errors.New("max queue wait time cannot be negative")
	}
	if c.MaxUploadQueueSize <= 0 {
		return errors.New("max dispatch batches must be greater than zero")
	}
	if c.DorisUploadWorkers <= 0 {
		return errors.New("workers must be greater than zero")
	}
	if strings.TrimSpace(c.LabelPrefix) == "" {
		return errors.New("label prefix cannot be empty")
	}
	if c.DorisUploadTimeout <= 0 {
		return errors.New("doris upload timeout must be greater than zero")
	}
	if c.DorisUploadRequestTimeout < minDorisUploadRequestTimeout {
		return errors.New("doris upload request timeout must be at least 10 seconds")
	}
	if c.StatusPollTimeout <= 0 {
		return errors.New("status poll timeout must be greater than zero")
	}
	if c.FakeSendDelay < 0 {
		return errors.New("fake send delay cannot be negative")
	}
	return nil
}

func validateEndpointURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return errors.New("endpoint must be a valid URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("endpoint must use http or https")
	}
	if parsed.Host == "" {
		return errors.New("endpoint must include host[:port]")
	}
	if parsed.User != nil {
		return errors.New("endpoint must not include user info")
	}
	if parsed.RawQuery != "" {
		return errors.New("endpoint must not include query parameters")
	}
	if parsed.Fragment != "" {
		return errors.New("endpoint must not include fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return errors.New("endpoint must be http(s)://host[:port] with no path")
	}
	return nil
}

func validateStreamLoadURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return errors.New("stream load url must be a valid URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("stream load url must use http or https")
	}
	if parsed.Host == "" {
		return errors.New("stream load url must include host[:port]")
	}
	if parsed.Fragment != "" {
		return errors.New("stream load url must not include fragment")
	}
	return nil
}

func streamLoadURLHasSuffix(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "_stream_load")
}

// buildHTTPClient constructs the http.Client for stream load requests.
// If cfg.HTTPClient is already set, it patches Timeout and CheckRedirect only
// when those fields are zero/nil. Otherwise it builds a new client with TLS
// config derived from TLSSkipVerify and TLSCACertPath.
func buildHTTPClient(cfg Config) (*http.Client, error) {
	// Doris FE redirects stream load requests to a BE node via 307. Go strips
	// Authorization on cross-host redirects by default. Re-apply it only for the
	// single first redirect (FE→BE), which we trust because we chose the FE endpoint.
	// Further hops are unexpected and revert to Go's default strip behaviour to
	// preserve credential-leak protection against untrusted redirect targets.
	redirect := func(req *http.Request, via []*http.Request) error {
		if len(via) > 10 {
			return errors.New("too many redirects")
		}
		if len(via) == 1 {
			if auth := via[0].Header.Get("Authorization"); auth != "" && req.Header.Get("Authorization") == "" {
				req.Header.Set("Authorization", auth)
			}
		}
		return nil
	}

	transport, err := buildHTTPTransport(cfg)
	if err != nil {
		return nil, err
	}

	if cfg.HTTPClient != nil {
		client := *cfg.HTTPClient
		if client.Timeout == 0 {
			client.Timeout = cfg.DorisUploadRequestTimeout
		}
		if client.CheckRedirect == nil {
			client.CheckRedirect = redirect
		}
		if transport != nil {
			client.Transport = transport
		}
		return &client, nil
	}

	return &http.Client{
		Timeout:       cfg.DorisUploadRequestTimeout,
		Transport:     transport,
		CheckRedirect: redirect,
	}, nil
}

func buildHTTPTransport(cfg Config) (http.RoundTripper, error) {
	baseTransport := cfg.HTTPClient
	if baseTransport == nil || baseTransport.Transport == nil {
		base, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			if cfg.TLSSkipVerify || cfg.TLSCACertPath != "" {
				return &http.Transport{
					TLSClientConfig: &tls.Config{
						InsecureSkipVerify: cfg.TLSSkipVerify, //nolint:gosec
					},
				}, nil
			}
			return nil, nil
		}
		baseTransport = &http.Client{Transport: base.Clone()}
	}

	transport, ok := baseTransport.Transport.(*http.Transport)
	if !ok {
		if cfg.TLSSkipVerify || cfg.TLSCACertPath != "" {
			return nil, errors.New("http client transport must be *http.Transport when TLS settings are configured")
		}
		return baseTransport.Transport, nil
	}

	cloned := transport.Clone()
	if cfg.TLSSkipVerify || cfg.TLSCACertPath != "" {
		tlsCfg := cloned.TLSClientConfig
		if tlsCfg == nil {
			tlsCfg = &tls.Config{}
		} else {
			tlsCfg = tlsCfg.Clone()
		}
		tlsCfg.InsecureSkipVerify = cfg.TLSSkipVerify //nolint:gosec
		if cfg.TLSCACertPath != "" {
			pool, err := loadCACertPool(cfg.TLSCACertPath)
			if err != nil {
				return nil, err
			}
			tlsCfg.RootCAs = pool
		}
		cloned.TLSClientConfig = tlsCfg
	}
	return cloned, nil
}

func decodePKCS12(data []byte) ([]*pem.Block, error) {
	return pkcs12.ToPEM(data, "")
}

func loadCACertPool(certPath string) (*x509.CertPool, error) {
	data, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("reading TLS CA cert: %w", err)
	}

	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if pool.AppendCertsFromPEM(data) {
		return pool, nil
	}

	blocks, pkcs12Err := decodePKCS12(data)
	if pkcs12Err == nil {
		added := false
		for _, block := range blocks {
			if block.Type == "CERTIFICATE" {
				if pool.AppendCertsFromPEM(pem.EncodeToMemory(block)) {
					added = true
				}
			}
		}
		if added {
			return pool, nil
		}
		return nil, errors.New("no certificates found in PKCS12 CA cert file")
	}

	return nil, fmt.Errorf("tls ca cert file is neither valid PEM nor valid PKCS12: pem parse failed and pkcs12 decode failed: %w", pkcs12Err)
}
