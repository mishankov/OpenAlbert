package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"jarvis/internal/logpipe"
)

func main() {
	target := os.Getenv("VICTORIALOGS_INSERT_URL")
	if target == "" {
		target = "http://victorialogs:9428/insert/jsonline?_stream_fields=service,container,project&_msg_field=_msg&_time_field=_time"
	}
	server := &http.Server{Addr: ":9429", Handler: &logpipe.Handler{Target: target}, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, MaxHeaderBytes: 32 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	// Do not log individual ingestion failures into our own input: that would create a loop.
	// Counters are exposed in /healthz; failed batches return non-2xx for driver retries.
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		os.Exit(1)
	}
}
