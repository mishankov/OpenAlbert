package main

import (
	"context"
	"log"
	"net/http"
	"openalbert/internal/sttserver"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	s := sttserver.New(os.Getenv("STT_MODEL"), os.Getenv("STT_API_KEY"), "http://127.0.0.1:8081")
	server := &http.Server{Addr: ":8080", Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: time.Minute, WriteTimeout: 11 * time.Minute, IdleTimeout: time.Minute}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(c)
	}()
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal("STT HTTP server failed")
	}
}
