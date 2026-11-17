// Command llm-slo is a small proxy that measures LLM API calls the way an
// SRE would: latency, time to first chunk, errors, tokens and cost per app,
// exported to Prometheus, with SLO burn-rate alerts generated from config.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/OTA-EITA/llm-slo/internal/config"
	"github.com/OTA-EITA/llm-slo/internal/proxy"
	"github.com/OTA-EITA/llm-slo/internal/rules"
)

// version is set with -ldflags "-X main.version=..." in builds; go install
// leaves it empty and the module version is used instead.
var version = ""

func init() {
	if version != "" {
		return
	}
	version = "dev"
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}
}

const usageText = `llm-slo %s

Usage:
  llm-slo serve -config llm-slo.json     run the proxy and the /metrics endpoint
  llm-slo rules -config llm-slo.json     print Prometheus rules for the SLOs and budgets
          [-format prometheus|k8s] [-name llm-slo]
  llm-slo check -config llm-slo.json     validate the config and exit
  llm-slo version
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(stderr, usageText, version)
		return 2
	}
	cmd, args := args[0], args[1:]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "llm-slo.json", "config file")
	format := fs.String("format", "prometheus", "rules: prometheus or k8s")
	name := fs.String("name", "llm-slo", "rules: PrometheusRule name for -format k8s")

	switch cmd {
	case "version", "-version", "--version":
		fmt.Fprintln(stdout, version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprintf(stdout, usageText, version)
		return 0
	case "serve", "rules", "check":
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n"+usageText, cmd, version)
		return 2
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	switch cmd {
	case "check":
		fmt.Fprintf(stdout, "%s: ok (%d upstreams, %d prices, %d SLOs, %d budgets)\n",
			*cfgPath, len(cfg.Upstreams), len(cfg.Prices), len(cfg.SLOs), len(cfg.Budgets))
		return 0
	case "rules":
		f := rules.Format(*format)
		if f != rules.Plain && f != rules.K8s {
			fmt.Fprintf(stderr, "-format must be prometheus or k8s\n")
			return 2
		}
		if err := rules.Write(stdout, cfg, f, *name); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	if err := serve(cfg, stderr); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func serve(cfg *config.Config, stderr io.Writer) error {
	if len(cfg.Upstreams) == 0 {
		return errors.New("no upstreams configured")
	}
	logger := log.New(stderr, "", 0)
	s, err := proxy.New(cfg, version, proxy.WithLogger(logger))
	if err != nil {
		return err
	}
	metricsMux := http.NewServeMux()
	metricsMux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		s.Registry.WriteText(w)
	})
	srv := &http.Server{Addr: cfg.Listen, Handler: s, ReadHeaderTimeout: 10 * time.Second}
	msrv := &http.Server{Addr: cfg.MetricsListen, Handler: metricsMux, ReadHeaderTimeout: 10 * time.Second}

	errc := make(chan error, 2)
	go func() { errc <- srv.ListenAndServe() }()
	go func() { errc <- msrv.ListenAndServe() }()
	logger.Printf(`{"msg":"llm-slo started","version":%q,"listen":%q,"metrics_listen":%q,"upstreams":%d}`,
		version, cfg.Listen, cfg.MetricsListen, len(cfg.Upstreams))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// let streams in flight finish
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = msrv.Shutdown(shutdown)
	return srv.Shutdown(shutdown)
}
