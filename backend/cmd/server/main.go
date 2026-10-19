package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"mediaex/backend/internal/app"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)
	c, e := app.LoadConfig()
	if e != nil {
		return e
	}
	for _, tool := range []string{"ffprobe", "ffmpeg"} {
		if _, e = exec.LookPath(tool); e != nil {
			return e
		}
	}
	db, e := app.OpenDB(c.DataDir)
	if e != nil {
		return e
	}
	defer db.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if e = app.Bootstrap(ctx, db, c.AdminName, c.AdminPassword); e != nil {
		return e
	}
	converter := app.NewConverter(c, db, log)
	scanner := app.NewScanner(c, db, log)
	scanner.Derive = converter.PlanFor
	server := app.NewServer(c, db, log)
	server.Scanner = scanner
	httpServer := &http.Server{Addr: c.ListenAddr, Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 16384}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); scanner.Run(ctx) }()
	go func() { defer wg.Done(); converter.Run(ctx) }()
	errs := make(chan error, 1)
	go func() { log.Info("server listening", "address", c.ListenAddr); errs <- httpServer.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case e = <-errs:
		stop()
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdown)
	stop()
	wg.Wait()
	if e == http.ErrServerClosed {
		return nil
	}
	return e
}
