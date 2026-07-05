package config

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestParseFlagEnvPrecedence(t *testing.T) {
	t.Parallel()

	env := map[string]string{
		envListen:              ":9000",
		envLocalURL:            "http://env-local",
		envTRAPIKey:            "env-key",
		envTRBaseURL:           "http://env-tr/v1",
		envTRCatalogURL:        "http://env-catalog/v1",
		envLocalMaxConcurrency: "9",
		envLocalQueueWait:      "250ms",
		envBurstOnError:        "false",
		envToken:               "env-token",
	}
	cfg, err := Parse([]string{
		"-listen", ":9999",
		"-local-url", "http://flag-local",
		"-tr-catalog-url", "http://flag-catalog/v1",
		"-local-max-concurrency", "2",
		"-local-queue-wait", "1s",
		"-burst-on-error=true",
	}, envLookup(env), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.Listen != ":9999" || cfg.LocalURL != "http://flag-local" || cfg.LocalMaxConcurrency != 2 {
		t.Fatalf("flag precedence failed: %#v", cfg)
	}
	if cfg.TRAPIKey != "env-key" || cfg.TRBaseURL != "http://env-tr/v1" || cfg.Token != "env-token" {
		t.Fatalf("env fallback failed: %#v", cfg)
	}
	if cfg.TRCatalogURL != "http://flag-catalog/v1" {
		t.Fatalf("tr catalog URL = %q, want flag value", cfg.TRCatalogURL)
	}
	if cfg.LocalQueueWait != time.Second || !cfg.BurstOnError {
		t.Fatalf("duration/bool parse failed: %#v", cfg)
	}
}

func TestParseValidationAndUsage(t *testing.T) {
	t.Parallel()

	if _, err := Parse(nil, envLookup(nil), &bytes.Buffer{}); err == nil {
		t.Fatal("Parse() without upstream succeeded")
	}
	var usage bytes.Buffer
	if _, err := Parse([]string{"-h"}, envLookup(nil), &usage); err == nil {
		t.Fatal("Parse(-h) error = nil, want flag.ErrHelp")
	}
	for _, want := range []string{"-listen", "BURSTY_LOCAL_URL", "TRUSTEDROUTER_API_KEY", "BURSTY_TR_CATALOG_URL", "BURSTY_BURST_ON_ERROR"} {
		if !strings.Contains(usage.String(), want) {
			t.Fatalf("usage missing %q:\n%s", want, usage.String())
		}
	}
}

func envLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
