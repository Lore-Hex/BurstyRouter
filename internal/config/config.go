package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	trustedrouter "github.com/Lore-Hex/trusted-router-go"
)

const (
	envListen              = "BURSTY_LISTEN"
	envLocalURL            = "BURSTY_LOCAL_URL"
	envTRAPIKey            = "TRUSTEDROUTER_API_KEY"
	envTRBaseURL           = "BURSTY_TR_BASE_URL"
	envTRCatalogURL        = "BURSTY_TR_CATALOG_URL"
	envLocalMaxConcurrency = "BURSTY_LOCAL_MAX_CONCURRENCY"
	envLocalQueueWait      = "BURSTY_LOCAL_QUEUE_WAIT"
	envBurstOnError        = "BURSTY_BURST_ON_ERROR"
	envBurstFallbackModel  = "BURSTY_BURST_FALLBACK_MODEL"
	envToken               = "BURSTY_TOKEN"
	envAliases             = "BURSTY_ALIASES"
)

// DefaultTRCatalogURL is the public TrustedRouter control-plane catalog base URL.
const DefaultTRCatalogURL = "https://trustedrouter.com/v1"

// Config is the complete runtime configuration for a BurstyRouter process.
type Config struct {
	Listen              string
	LocalURL            string
	TRAPIKey            string
	TRBaseURL           string
	TRCatalogURL        string
	LocalMaxConcurrency int
	LocalQueueWait      time.Duration
	BurstOnError        bool
	BurstFallbackModel  string
	Token               string
	Aliases             map[string]string
}

// HasLocal reports whether local upstream routing is configured.
func (c Config) HasLocal() bool {
	return strings.TrimSpace(c.LocalURL) != ""
}

// HasTrustedRouter reports whether TrustedRouter routing is configured.
func (c Config) HasTrustedRouter() bool {
	return strings.TrimSpace(c.TRAPIKey) != ""
}

// Parse parses flags with environment-variable fallbacks. Flag values win over
// environment values because env/default values are installed as flag defaults.
func Parse(args []string, lookupEnv func(string) (string, bool), output io.Writer) (Config, error) {
	cfg, err := defaultsFromEnv(lookupEnv)
	if err != nil {
		return Config{}, err
	}

	fs := flag.NewFlagSet("burstyrouter", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&cfg.Listen, "listen", cfg.Listen, "bind address")
	fs.StringVar(&cfg.LocalURL, "local-url", cfg.LocalURL, "local OpenAI-compatible base URL")
	fs.StringVar(&cfg.TRAPIKey, "tr-api-key", cfg.TRAPIKey, "TrustedRouter API key")
	fs.StringVar(&cfg.TRBaseURL, "tr-base-url", cfg.TRBaseURL, "TrustedRouter OpenAI-compatible base URL")
	fs.StringVar(&cfg.TRCatalogURL, "tr-catalog-url", cfg.TRCatalogURL, "TrustedRouter public catalog base URL")
	fs.IntVar(&cfg.LocalMaxConcurrency, "local-max-concurrency", cfg.LocalMaxConcurrency, "in-flight cap on local upstream")
	fs.DurationVar(&cfg.LocalQueueWait, "local-queue-wait", cfg.LocalQueueWait, "how long to wait for a local slot before bursting")
	fs.BoolVar(&cfg.BurstOnError, "burst-on-error", cfg.BurstOnError, "burst to TrustedRouter on local connect error/timeout/429/5xx/404-model")
	fs.StringVar(&cfg.BurstFallbackModel, "burst-fallback-model", cfg.BurstFallbackModel, "TrustedRouter model to use when bursting an unmapped local-native model")
	fs.StringVar(&cfg.Token, "token", cfg.Token, "optional inbound bearer token")
	fs.Var(aliasMapValue{values: cfg.Aliases}, "alias", "model alias CLOUD-id=LOCAL-model; repeatable")
	fs.Usage = func() {
		fmt.Fprintln(output, "Usage: burstyrouter [flags]")
		fmt.Fprintln(output)
		fmt.Fprintln(output, "Flags:")
		fmt.Fprintln(output, "  -listen                    env BURSTY_LISTEN                  default :8383")
		fmt.Fprintln(output, "  -local-url                 env BURSTY_LOCAL_URL               default \"\"")
		fmt.Fprintln(output, "  -tr-api-key                env TRUSTEDROUTER_API_KEY          default \"\"")
		fmt.Fprintf(output, "  -tr-base-url               env BURSTY_TR_BASE_URL             default %s\n", trustedrouter.DefaultAPIBaseURL)
		fmt.Fprintf(output, "  -tr-catalog-url            env BURSTY_TR_CATALOG_URL          default %s\n", DefaultTRCatalogURL)
		fmt.Fprintln(output, "  -local-max-concurrency     env BURSTY_LOCAL_MAX_CONCURRENCY   default 4")
		fmt.Fprintln(output, "  -local-queue-wait          env BURSTY_LOCAL_QUEUE_WAIT        default 0s")
		fmt.Fprintln(output, "  -burst-on-error            env BURSTY_BURST_ON_ERROR          default true")
		fmt.Fprintln(output, "  -burst-fallback-model      env BURSTY_BURST_FALLBACK_MODEL    default \"\"")
		fmt.Fprintln(output, "  -token                     env BURSTY_TOKEN                   default \"\"")
		fmt.Fprintln(output, "  -alias                     env BURSTY_ALIASES                 default \"\"")
	}
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if err := validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func defaultsFromEnv(lookupEnv func(string) (string, bool)) (Config, error) {
	cfg := Config{
		Listen:              ":8383",
		TRBaseURL:           trustedrouter.DefaultAPIBaseURL,
		TRCatalogURL:        DefaultTRCatalogURL,
		LocalMaxConcurrency: 4,
		LocalQueueWait:      0,
		BurstOnError:        true,
		Aliases:             map[string]string{},
	}

	if value, ok := lookupEnv(envListen); ok {
		cfg.Listen = value
	}
	if value, ok := lookupEnv(envLocalURL); ok {
		cfg.LocalURL = value
	}
	if value, ok := lookupEnv(envTRAPIKey); ok {
		cfg.TRAPIKey = value
	}
	if value, ok := lookupEnv(envTRBaseURL); ok {
		cfg.TRBaseURL = value
	}
	if value, ok := lookupEnv(envTRCatalogURL); ok {
		cfg.TRCatalogURL = value
	}
	if value, ok := lookupEnv(envLocalMaxConcurrency); ok {
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", envLocalMaxConcurrency, err)
		}
		cfg.LocalMaxConcurrency = parsed
	}
	if value, ok := lookupEnv(envLocalQueueWait); ok {
		parsed, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", envLocalQueueWait, err)
		}
		cfg.LocalQueueWait = parsed
	}
	if value, ok := lookupEnv(envBurstOnError); ok {
		parsed, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", envBurstOnError, err)
		}
		cfg.BurstOnError = parsed
	}
	if value, ok := lookupEnv(envBurstFallbackModel); ok {
		cfg.BurstFallbackModel = value
	}
	if value, ok := lookupEnv(envToken); ok {
		cfg.Token = value
	}
	if value, ok := lookupEnv(envAliases); ok {
		if err := parseAliasList(value, cfg.Aliases); err != nil {
			return Config{}, fmt.Errorf("%s: %w", envAliases, err)
		}
	}

	return cfg, nil
}

func validate(cfg Config) error {
	if strings.TrimSpace(cfg.LocalURL) == "" && strings.TrimSpace(cfg.TRAPIKey) == "" {
		return errors.New("at least one of -local-url or -tr-api-key is required")
	}
	if strings.TrimSpace(cfg.Listen) == "" {
		return errors.New("-listen must not be empty")
	}
	if cfg.LocalMaxConcurrency < 1 {
		return errors.New("-local-max-concurrency must be at least 1")
	}
	if cfg.LocalQueueWait < 0 {
		return errors.New("-local-queue-wait must not be negative")
	}
	return nil
}

type aliasMapValue struct {
	values map[string]string
}

func (v aliasMapValue) String() string {
	if len(v.values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(v.values))
	for key := range v.values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+v.values[key])
	}
	return strings.Join(parts, ",")
}

func (v aliasMapValue) Set(value string) error {
	from, to, err := parseAliasPair(value)
	if err != nil {
		return err
	}
	if _, ok := v.values[from]; ok {
		return fmt.Errorf("duplicate alias %q", from)
	}
	v.values[from] = to
	return nil
}

func parseAliasList(value string, aliases map[string]string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parser := aliasMapValue{values: aliases}
	for _, part := range strings.Split(value, ",") {
		if err := parser.Set(part); err != nil {
			return err
		}
	}
	return nil
}

func parseAliasPair(value string) (string, string, error) {
	value = strings.TrimSpace(value)
	if strings.Count(value, "=") != 1 {
		return "", "", fmt.Errorf("alias %q must have from=to shape", value)
	}
	from, to, _ := strings.Cut(value, "=")
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if from == "" || to == "" {
		return "", "", fmt.Errorf("alias %q must have non-empty from and to", value)
	}
	return from, to, nil
}
