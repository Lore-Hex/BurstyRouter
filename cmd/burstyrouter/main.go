package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Lore-Hex/BurstyRouter/internal/autodetect"
	"github.com/Lore-Hex/BurstyRouter/internal/config"
	"github.com/Lore-Hex/BurstyRouter/internal/proxy"
)

var version = "dev"

type localBannerInfo struct {
	URL             string
	Flavor          string
	ModelCount      int
	ModelCountKnown bool
	Autodetected    bool
}

func main() {
	cfg, err := config.Parse(os.Args[1:], os.LookupEnv, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.PrintVersion {
		fmt.Fprintln(os.Stdout, version)
		return
	}

	localInfo := localBannerInfo{}
	if !cfg.HasLocal() && !cfg.NoAutodetect {
		if result, ok := autodetect.Detect(context.Background(), autodetect.DefaultProbes(os.LookupEnv), 0, nil); ok {
			cfg.LocalURL = result.URL
			localInfo = localBannerInfo{
				URL:             result.URL,
				Flavor:          result.Name,
				ModelCount:      result.ModelCount,
				ModelCountKnown: true,
				Autodetected:    true,
			}
			log.Printf("bursty autodetect: found %s at %s with %d models", result.Name, result.URL, result.ModelCount)
		} else if cfg.HasTrustedRouter() {
			log.Printf("bursty autodetect: no local server found; running pure TrustedRouter passthrough mode")
		}
	} else if !cfg.HasLocal() && cfg.HasTrustedRouter() {
		log.Printf("bursty autodetect: disabled; running pure TrustedRouter passthrough mode")
	}
	if err := config.ValidateRuntime(cfg); err != nil {
		log.Fatal(err)
	}
	if cfg.HasLocal() && localInfo.URL == "" {
		localInfo = inspectConfiguredLocal(context.Background(), cfg.LocalURL)
	}

	handler, err := proxy.New(cfg)
	if err != nil {
		log.Fatalf("proxy: %v", err)
	}
	defer handler.Close()
	printBootBanner(os.Stderr, cfg, localInfo, handler.SavingsTotals())

	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("burstyrouter listening on %s", cfg.Listen)

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for range hup {
			handler.HandleSIGHUP()
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("server shutdown: %v", err)
			_ = server.Close()
		}
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server: %v", err)
		}
	}
}

func inspectConfiguredLocal(ctx context.Context, rawURL string) localBannerInfo {
	info := localBannerInfo{
		URL:             strings.TrimSpace(rawURL),
		Flavor:          autodetect.GuessFlavor(rawURL),
		ModelCountKnown: false,
	}
	if normalized, err := autodetect.NormalizeBase(rawURL); err == nil {
		info.URL = normalized
	}
	result, err := autodetect.ProbeServer(ctx, autodetect.Probe{Name: info.Flavor, URL: rawURL}, autodetect.DefaultProbeTimeout, nil)
	if err != nil {
		return info
	}
	info.URL = result.URL
	info.Flavor = result.Name
	info.ModelCount = result.ModelCount
	info.ModelCountKnown = true
	return info
}

func printBootBanner(w io.Writer, cfg config.Config, local localBannerInfo, savings proxy.SavingsTotals) {
	fmt.Fprintf(w, "BurstyRouter %s\n", version)
	if local.URL == "" {
		fmt.Fprintln(w, "local: disabled (pure cloud passthrough)")
	} else {
		source := "configured"
		if local.Autodetected {
			source = "detected"
		}
		models := "models unknown"
		if local.ModelCountKnown {
			models = fmt.Sprintf("%d models", local.ModelCount)
		}
		fmt.Fprintf(w, "local: %s %s at %s (%s)\n", source, local.Flavor, local.URL, models)
	}
	fmt.Fprintf(w, "cloud: %s\n", cloudDisplay(cfg))
	if mode := modeDisplay(cfg); mode != "" {
		fmt.Fprintf(w, "mode: %s\n", mode)
	}
	if savings.HasHistory {
		fmt.Fprintf(w, "savings: saved $%s (ref: %s), cloud spend $%s\n", formatUSDMicro(savings.SavedUSDMicro), savings.TopReference, formatUSDMicro(savings.CloudSpendUSDMicro))
	}
	fmt.Fprintln(w, "Point your tools at http://localhost:8383/v1")
}

func cloudDisplay(cfg config.Config) string {
	if !cfg.HasTrustedRouter() || cfg.Cloud == config.CloudOff {
		return "disabled"
	}
	parsed, err := url.Parse(cfg.TRBaseURL)
	if err != nil || parsed.Host == "" {
		return strings.TrimSpace(cfg.TRBaseURL)
	}
	return parsed.Host
}

func modeDisplay(cfg config.Config) string {
	parts := []string{}
	if cfg.Cloud != config.CloudAuto {
		parts = append(parts, "cloud="+string(cfg.Cloud))
	}
	if cfg.MaxCloudSpendMicro > 0 {
		parts = append(parts, "max-cloud-spend=$"+formatUSDMicro(cfg.MaxCloudSpendMicro)+"/day")
	}
	return strings.Join(parts, ", ")
}

func formatUSDMicro(value int64) string {
	return fmt.Sprintf("%.6f", float64(value)/1_000_000)
}
