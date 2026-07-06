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
		envLocalSlowAfter:      "750ms",
		envBurstOnError:        "false",
		envBurstFallbackModel:  "openai/gpt-4o-mini",
		envToken:               "env-token",
		envAliases:             "gpt-4o=llama3.2, anthropic/claude-haiku-4.5=llama3.1",
		envSavingsReference:    "openai/gpt-4o-mini",
		envStateFile:           "/tmp/env-state.json",
		envCloud:               "explicit",
		envMaxCloudSpend:       "1.25",
	}
	cfg, err := Parse([]string{
		"-listen", ":9999",
		"-local-url", "http://flag-local",
		"-tr-catalog-url", "http://flag-catalog/v1",
		"-local-max-concurrency", "2",
		"-local-queue-wait", "1s",
		"-local-slow-after", "2s",
		"-burst-fallback-model", "openai/gpt-4o",
		"-burst-on-error=true",
		"-alias", "openai/gpt-4.1=qwen2.5-coder:32b",
		"-savings-reference", "openai/gpt-4.1",
		"-state-file", "/tmp/flag-state.json",
		"-cloud", "off",
		"-max-cloud-spend", "2.50",
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
	if cfg.LocalQueueWait != time.Second || cfg.LocalSlowAfter != 2*time.Second || !cfg.BurstOnError {
		t.Fatalf("duration/bool parse failed: %#v", cfg)
	}
	if cfg.BurstFallbackModel != "openai/gpt-4o" {
		t.Fatalf("burst fallback = %q, want flag value", cfg.BurstFallbackModel)
	}
	if cfg.SavingsReference != "openai/gpt-4.1" || cfg.StateFile != "/tmp/flag-state.json" || cfg.Cloud != CloudOff {
		t.Fatalf("new flag precedence failed: %#v", cfg)
	}
	if cfg.MaxCloudSpendMicro != 2_500_000 {
		t.Fatalf("max cloud spend micro = %d, want 2500000", cfg.MaxCloudSpendMicro)
	}
	wantAliases := map[string]string{
		"gpt-4o":                     "llama3.2",
		"anthropic/claude-haiku-4.5": "llama3.1",
		"openai/gpt-4.1":             "qwen2.5-coder:32b",
	}
	for from, to := range wantAliases {
		if cfg.Aliases[from] != to {
			t.Fatalf("alias %q = %q, want %q; aliases=%#v", from, cfg.Aliases[from], to, cfg.Aliases)
		}
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
	for _, want := range []string{"-listen", "BURSTY_LOCAL_URL", "TRUSTEDROUTER_API_KEY", "BURSTY_TR_CATALOG_URL", "BURSTY_LOCAL_SLOW_AFTER", "BURSTY_BURST_ON_ERROR", "BURSTY_BURST_FALLBACK_MODEL", "BURSTY_ALIASES", "BURSTY_SAVINGS_REFERENCE", "BURSTY_STATE_FILE", "BURSTY_CLOUD", "BURSTY_MAX_CLOUD_SPEND"} {
		if !strings.Contains(usage.String(), want) {
			t.Fatalf("usage missing %q:\n%s", want, usage.String())
		}
	}

	if _, err := Parse([]string{"-local-url", "http://local", "-cloud", "maybe"}, envLookup(nil), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "auto, explicit, off") {
		t.Fatalf("invalid cloud mode error = %v", err)
	}
	if _, err := Parse([]string{"-local-url", "http://local", "-max-cloud-spend", "-1"}, envLookup(nil), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("invalid max cloud spend error = %v", err)
	}
	if _, err := Parse([]string{"-local-url", "http://local", "-local-slow-after", "-1s"}, envLookup(nil), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("invalid local slow after error = %v", err)
	}
}

func TestParseAliases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		env     map[string]string
		args    []string
		want    map[string]string
		wantErr string
	}{
		{
			name: "env comma list",
			env: map[string]string{
				envLocalURL: "http://local",
				envAliases:  " gpt-4o = llama3.2 , anthropic/claude-haiku-4.5=qwen2.5 ",
			},
			want: map[string]string{
				"gpt-4o":                     "llama3.2",
				"anthropic/claude-haiku-4.5": "qwen2.5",
			},
		},
		{
			name: "repeat flags",
			env:  map[string]string{envLocalURL: "http://local"},
			args: []string{"-alias", "gpt-4o=llama3.2", "-alias", "openai/gpt-4.1=qwen"},
			want: map[string]string{
				"gpt-4o":         "llama3.2",
				"openai/gpt-4.1": "qwen",
			},
		},
		{
			name:    "invalid shape",
			env:     map[string]string{envLocalURL: "http://local"},
			args:    []string{"-alias", "gpt-4o"},
			wantErr: "from=to",
		},
		{
			name:    "empty target",
			env:     map[string]string{envLocalURL: "http://local"},
			args:    []string{"-alias", "gpt-4o="},
			wantErr: "non-empty",
		},
		{
			name:    "duplicate env key",
			env:     map[string]string{envLocalURL: "http://local", envAliases: "gpt-4o=llama3,gpt-4o=qwen"},
			wantErr: "duplicate alias",
		},
		{
			name:    "duplicate flag key",
			env:     map[string]string{envLocalURL: "http://local"},
			args:    []string{"-alias", "gpt-4o=llama3", "-alias", "gpt-4o=qwen"},
			wantErr: "duplicate alias",
		},
		{
			name:    "duplicate env and flag key",
			env:     map[string]string{envLocalURL: "http://local", envAliases: "gpt-4o=llama3"},
			args:    []string{"-alias", "gpt-4o=qwen"},
			wantErr: "duplicate alias",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Parse(tt.args, envLookup(tt.env), &bytes.Buffer{})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Parse() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			for from, to := range tt.want {
				if cfg.Aliases[from] != to {
					t.Fatalf("alias %q = %q, want %q; aliases=%#v", from, cfg.Aliases[from], to, cfg.Aliases)
				}
			}
			if len(cfg.Aliases) != len(tt.want) {
				t.Fatalf("aliases=%#v, want %#v", cfg.Aliases, tt.want)
			}
		})
	}
}

func envLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
