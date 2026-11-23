// SPDX-License-Identifier: GPL-3.0-or-later

// Command forgejo-berth gives each waiting Forgejo Actions job an ephemeral runner on a
// container slot of a fixed host where its learned memory peak fits.
//
//	forgejo-berth run [--config FILE] [--shadow]        the broker
//	forgejo-berth check [--config FILE]                 checks the configuration and the secrets
//	forgejo-berth slot [--settings-dir DIR] [REQUEST]   the slot's forced SSH command
//	forgejo-berth cycle [--settings-dir DIR] CONTAINER  one slot's reset cycle
//	forgejo-berth version
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"git.ciepiela.org/dawid/forgejo-berth/internal/broker"
	"git.ciepiela.org/dawid/forgejo-berth/internal/config"
	"git.ciepiela.org/dawid/forgejo-berth/internal/forgejo"
	"git.ciepiela.org/dawid/forgejo-berth/internal/history"
	"git.ciepiela.org/dawid/forgejo-berth/internal/provider"
	"git.ciepiela.org/dawid/forgejo-berth/internal/provider/docker"
	"git.ciepiela.org/dawid/forgejo-berth/internal/slot"
	"git.ciepiela.org/dawid/forgejo-berth/internal/web"
)

// version is set at build time (-ldflags "-X main.version=v1.2.3").
var version = "dev"

const (
	defaultConfig      = "/etc/forgejo-berth/config.yml"
	defaultSettingsDir = "/etc/forgejo-berth/slots"
	usage              = `usage: forgejo-berth run [--config FILE] [--shadow]
       forgejo-berth check [--config FILE]
       forgejo-berth slot [--settings-dir DIR] [REQUEST]
       forgejo-berth cycle [--settings-dir DIR] [--log-format text|json] CONTAINER
       forgejo-berth version`
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version", "--version":
		fmt.Fprintln(stdout, version)
		return 0
	case "run":
		return runBroker(args[1:], stderr)
	case "check":
		return check(args[1:], stdout, stderr)
	case "slot":
		return runSlot(args[1:], stdin, stdout, stderr)
	case "cycle":
		return runCycle(args[1:], stdout, stderr)
	}
	fmt.Fprintln(stderr, usage)
	return 2
}

func loadConfig(name string, args []string, stderr io.Writer, extra func(*flag.FlagSet)) (*config.Config, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", defaultConfig, "the configuration file")
	if extra != nil {
		extra(fs)
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	return config.Load(*path)
}

func logger(cfg *config.Config, w io.Writer) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.Log.Level))
	opts := &slog.HandlerOptions{Level: level}
	if cfg.Log.Format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// tokens reads the admin token and the trusted owners' tokens.
func tokens(cfg *config.Config) (string, map[string]string, error) {
	admin, err := config.ReadSecret(cfg.Forgejo.AdminTokenFile)
	if err != nil {
		return "", nil, fmt.Errorf("the admin token: %w", err)
	}
	owners := map[string]string{}
	for owner, file := range cfg.Trusted.Owners {
		if owners[owner], err = config.ReadSecret(file); err != nil {
			return "", nil, fmt.Errorf("the token of %s: %w", owner, err)
		}
	}
	return admin, owners, nil
}

func check(args []string, stdout, stderr io.Writer) int {
	cfg, err := loadConfig("check", args, stderr, nil)
	if err == nil {
		_, _, err = tokens(cfg)
	}
	if err == nil {
		err = checkFiles(cfg)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "the configuration is valid: %d hosts, %d trusted owners\n", len(cfg.Hosts), len(cfg.Trusted.Owners))
	return 0
}

// checkFiles checks that the provider's files are there: the SSH key, or the TLS files
// and the runner's configuration template.
func checkFiles(cfg *config.Config) error {
	var files, secrets []string
	p := cfg.Provider
	switch p.Type {
	case config.ProviderSSH:
		secrets = []string{p.SSH.KeyFile}
	case config.ProviderDocker:
		files = []string{p.Docker.TLS.CAFile, p.Docker.TLS.CertFile, p.Docker.ConfigTemplate}
		secrets = []string{p.Docker.TLS.KeyFile}
	}
	for _, f := range secrets {
		if f != "" {
			files = append(files, config.Secret(f))
		}
	}
	for _, f := range files {
		if f == "" {
			continue
		}
		if _, err := os.Stat(f); err != nil {
			return err
		}
	}
	return nil
}

func runBroker(args []string, stderr io.Writer) int {
	var shadow bool
	cfg, err := loadConfig("run", args, stderr, func(fs *flag.FlagSet) {
		fs.BoolVar(&shadow, "shadow", false, "compute and log decisions without acting on them")
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	cfg.Shadow = cfg.Shadow || shadow
	log := logger(cfg, stderr)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := serve(ctx, cfg, log); err != nil {
		log.Error("cannot run", "error", err)
		return 1
	}
	return 0
}

func serve(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	admin, owners, err := tokens(cfg)
	if err != nil {
		return err
	}
	if !cfg.Shadow {
		if err := os.MkdirAll(filepath.Dir(cfg.History.Path), 0o700); err != nil {
			return err
		}
	}
	hist, err := history.Open(cfg.History.Path, cfg.Shadow)
	if err != nil {
		return err
	}
	defer hist.Close()
	slots, stopSlots, err := newProvider(cfg, log)
	if err != nil {
		return err
	}
	defer stopSlots()
	b, err := broker.New(cfg, broker.Options{
		Forge:    forgejo.New(cfg.Forgejo.URL, admin, owners, nil).WithLog(log),
		Provider: slots,
		History:  hist,
		Log:      log,
	})
	if err != nil {
		return err
	}
	if cfg.Listen != "" {
		listener, err := net.Listen("tcp", cfg.Listen)
		if err != nil {
			return err
		}
		server := &http.Server{Handler: web.Handler(b), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("the HTTP server stopped", "error", err)
			}
		}()
		defer server.Close()
	}
	log.Info("started", "version", version, "hosts", len(cfg.Hosts), "shadow", b.Shadow())
	if err := b.Sweep(ctx); err != nil {
		log.Warn("the sweep of leftover runners failed", "error", err)
	}
	interval := time.Duration(cfg.IntervalSeconds * float64(time.Second))
	for {
		look(ctx, b, cfg, log)
		select {
		case <-ctx.Done():
			log.Info("stopped")
			return nil
		case <-time.After(interval):
		}
	}
}

// newProvider makes the configuration's provider, and what stops it: the docker provider
// runs the slots' cycles in this process.
func newProvider(cfg *config.Config, log *slog.Logger) (provider.Provider, func(), error) {
	switch cfg.Provider.Type {
	case config.ProviderDocker:
		hosted, err := docker.New(cfg, log)
		if err != nil {
			return nil, nil, err
		}
		return hosted, hosted.Stop, nil
	}
	return provider.NewSSH(cfg.Provider.SSH, nil), func() {}, nil
}

// look runs one tick; a failure (or a panic) is logged and the next look may succeed.
func look(ctx context.Context, b *broker.Broker, cfg *config.Config, log *slog.Logger) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("this look failed", "panic", fmt.Sprint(r))
		}
	}()
	if err := b.Tick(ctx); err != nil && ctx.Err() == nil {
		log.Error("this look failed", "error", err)
	}
	if cfg.MetricsFile != "" {
		if err := broker.WriteFile(cfg.MetricsFile, []byte(b.Snapshot().Metrics()), 0o644); err != nil {
			log.Error("the metrics file was not written", "error", err)
		}
	}
	if cfg.StateFile != "" {
		data, _ := json.MarshalIndent(b.Snapshot(), "", "  ")
		if err := broker.WriteFile(cfg.StateFile, append(data, '\n'), 0o644); err != nil {
			log.Error("the state file was not written", "error", err)
		}
	}
}

func runSlot(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("slot", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("settings-dir", defaultSettingsDir, "the slots' settings")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	request := os.Getenv("SSH_ORIGINAL_COMMAND")
	if fs.NArg() > 0 {
		request = fs.Arg(0)
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(stderr, "the request is one argument")
		return 1
	}
	return slot.Command(request, *dir, stdin, stdout, stderr)
}

func runCycle(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cycle", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("settings-dir", defaultSettingsDir, "the slots' settings")
	format := fs.String("log-format", "text", "text or json")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || !config.Name.MatchString(fs.Arg(0)) {
		fmt.Fprintln(stderr, "usage: forgejo-berth cycle [--settings-dir DIR] [--log-format text|json] CONTAINER")
		return 2
	}
	s := slot.LoadSlots(*dir)[fs.Arg(0)]
	if s == nil {
		fmt.Fprintf(stderr, "no slot %s in %s\n", fs.Arg(0), *dir)
		return 1
	}
	log := slog.New(slog.NewTextHandler(stdout, nil))
	if *format == "json" {
		log = slog.New(slog.NewJSONHandler(stdout, nil))
	}
	// A stop ends the cycle after the step in progress.
	var stopping atomic.Bool
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
	go func() {
		<-sig
		stopping.Store(true)
	}()
	system := slot.NewIncus(s)
	system.Out = stdout
	slot.NewCycle(s, system, slot.CycleOptions{Log: log, Stopping: stopping.Load}).Run()
	return 0
}
