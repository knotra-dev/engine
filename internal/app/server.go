// Package app wires the independently testable engine modules into one service.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/knotra-dev/engine/internal/adapters"
	"github.com/knotra-dev/engine/internal/api"
	"github.com/knotra-dev/engine/internal/contract"
	"github.com/knotra-dev/engine/internal/execution"
	"github.com/knotra-dev/engine/internal/executor"
	"github.com/knotra-dev/engine/internal/queue"
	"github.com/knotra-dev/engine/internal/store"
	storedb "github.com/knotra-dev/engine/internal/store/db"
	"github.com/knotra-dev/engine/internal/telemetry"
)

type Options struct {
	HostID           string
	SharedDataDir    string
	ExecutionWorkers int
	Listen           string
	DatabaseURL      string
	DataDir          string
	DockerHost       string
	HelperPath       string
	FirewallImage    string
	Token            string
	CORSOrigin       string
	TLSCert          string
	TLSKey           string
	Version          string
	Profiles         []string
}

func Serve(ctx context.Context, o Options, log *slog.Logger) (serveErr error) {
	if o.HostID == "" {
		o.HostID = "local"
	}
	if err := execution.ValidateHostID(o.HostID); err != nil {
		return err
	}
	if o.ExecutionWorkers == 0 {
		o.ExecutionWorkers = 16
	}
	if o.ExecutionWorkers < 1 {
		return errors.New("execution workers must be positive")
	}
	stopTelemetry, err := telemetry.Start(ctx, o.Version)
	if err != nil {
		return err
	}
	defer func() {
		ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = stopTelemetry(ctx)
	}()

	host, _, err := net.SplitHostPort(o.Listen)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	local := host == "localhost" || (ip != nil && ip.IsLoopback())
	if !local && (o.Token == "" || o.TLSCert == "" || o.TLSKey == "") {
		return errors.New("remote listeners require bearer authentication and TLS certificate/key")
	}
	profiles := map[string]contract.Profile{}

	for _, path := range o.Profiles {
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		p, d := contract.ParseProfile(b)
		if contract.HasErrors(d) {
			return fmt.Errorf("profile %s: %v", path, d)
		}
		if _, exists := profiles[p.Metadata.Name]; exists {
			return fmt.Errorf("duplicate profile %q", p.Metadata.Name)
		}
		profiles[p.Metadata.Name] = p
	}

	if len(profiles) == 0 {
		return errors.New("at least one --profile file is required")
	}
	db, err := store.Open(ctx, o.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	lease, err := db.AcquireLease(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = lease.Close(context.Background()) }()
	dataDir, err := filepath.Abs(o.DataDir)
	if err != nil {
		return err
	}
	sharedDir := dataDir
	if o.SharedDataDir != "" {
		sharedDir, err = filepath.Abs(o.SharedDataDir)
		if err != nil {
			return err
		}
	}
	artifacts := store.Artifacts{Root: filepath.Join(sharedDir, "artifacts"), Store: db}
	runner := &adapters.Runner{
		EngineID:      db.EngineID,
		DockerHost:    o.DockerHost,
		HelperPath:    o.HelperPath,
		WorkDir:       filepath.Join(dataDir, "work"),
		FirewallImage: o.FirewallImage,
	}
	defer func() { _ = runner.Close() }()
	srv := &api.Server{Store: db, Artifacts: artifacts, Profiles: profiles, Token: o.Token, Version: o.Version, CORSOrigin: o.CORSOrigin, HostID: o.HostID, Log: log, Admit: func(ctx context.Context, p *contract.Plan) error {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		return runner.Prepare(ctx, p)
	}}

	config := queue.Config{EngineID: db.EngineID, HostID: o.HostID, Logger: log, ExecutionWorkers: o.ExecutionWorkers}
	// The combined service holds the exclusive engine lease. Cluster host
	// identities and placement are enabled separately after acceptance.
	for _, profile := range profiles {
		duration, err := contract.Duration(profile.Spec.Limits.Timeout)
		if err != nil {
			return err
		}
		config.ExecutionTimeout = max(config.ExecutionTimeout, duration+time.Minute)
	}
	// Replacing a profile must not shorten an already admitted plan's delivery.
	micros, err := storedb.New(db.Pool).MaximumActiveExecutionDurationMicros(ctx)
	if err != nil {
		return err
	}
	if micros < 0 || micros > int64(365*24*time.Hour/time.Microsecond) {
		return errors.New("stored execution duration exceeds the supported admission bound")
	}
	config.ExecutionTimeout = max(config.ExecutionTimeout, time.Duration(micros)*time.Microsecond+time.Minute)
	config.Schema, err = storedb.New(db.Pool).CurrentSchema(ctx)
	if err != nil {
		return err
	}
	if err := queue.Migrate(ctx, db.Pool, config.Schema); err != nil {
		return err
	}
	outcomes, err := execution.OpenOutcomeFiles(filepath.Join(sharedDir, "outcomes"))
	if err != nil {
		return err
	}
	defer func() { _ = outcomes.Close() }()
	native := &queue.Runtime{Store: db, Host: &executor.Host{Store: db, Artifacts: artifacts, Runner: runner},
		Outcomes: outcomes, WorkerID: uuid.NewString(), HostID: config.HostID}
	native.Client, err = queue.New(db.Pool, config, native.Handlers())
	if err != nil {
		return err
	}
	if err := native.Start(ctx); err != nil {
		return err
	}
	defer func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if stopErr := native.Stop(stopCtx); stopErr != nil {
			log.Error("execution shutdown failed", "engineId", db.EngineID, "hostId", native.HostID, "workerId", native.WorkerID, "error", telemetry.RedactError(stopErr))
			serveErr = errors.Join(serveErr, stopErr)
		}
	}()
	srv.Wake = native.CommandWake
	srv.ExecutionTimeout = config.ExecutionTimeout
	requestCtx, stopRequests := context.WithCancel(ctx)
	defer stopRequests()
	server := &http.Server{
		Addr:              o.Listen,
		Handler:           telemetry.HTTP(srv.Handler()),
		BaseContext:       func(net.Listener) context.Context { return requestCtx },
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", o.Listen)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	serviceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	leaseFailed := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-serviceCtx.Done():
				return
			case <-ticker.C:
			}

			checkCtx, stop := context.WithTimeout(serviceCtx, 5*time.Second)
			err := lease.Check(checkCtx)
			stop()
			if err != nil {
				if serviceCtx.Err() == nil {
					leaseFailed <- err
				}
				return
			}
		}
	}()
	failures := make(chan error, 1)
	go func() {
		if o.TLSCert != "" {
			failures <- server.ServeTLS(listener, o.TLSCert, o.TLSKey)
		} else {
			failures <- server.Serve(listener)
		}
	}()
	log.Info(
		"Knotra engine listening",
		"address",
		listener.Addr().String(),
		"engineId",
		db.EngineID,
		"backend",
		execution.BackendRiver,
	)

serveLoop:
	for {
		select {
		case <-ctx.Done():
			break serveLoop
		case problem := <-native.Failure:
			log.Warn("execution background check failed", "engineId", db.EngineID, "hostId", native.HostID, "workerId", native.WorkerID, "error", telemetry.RedactError(problem))
		case <-native.Done():
			err = native.Err()
			break serveLoop
		case err = <-leaseFailed:
			log.Error("engine ownership connection lost; stopping service")
			break serveLoop
		case err = <-failures:
			break serveLoop
		}
	}

	// Long-lived event streams must finish before Shutdown can drain connections.
	stopRequests()
	shutdownCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	shutdownErr := server.Shutdown(shutdownCtx)
	cancel()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return shutdownErr
}
