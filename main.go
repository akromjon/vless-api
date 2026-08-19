package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	settings, err := loadSettings()
	if err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}
	runtime := CommandRuntime{
		XrayBinary:      settings.XrayBinary,
		SystemctlBinary: settings.SystemctlBinary,
		ServiceName:     settings.XrayService,
		Timeout:         15 * time.Second,
	}
	store := NewConfigStore(settings.XrayConfigFile, settings.InboundTag, settings.Flow, runtime)
	if _, err := store.List(); err != nil {
		log.Fatalf("cannot load managed Xray users: %v", err)
	}

	api := NewAPIServer(settings, store, runtime)
	server := &http.Server{
		Addr:              settings.APIListenAddress(),
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	shutdownSignal, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-shutdownSignal.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("HTTP shutdown failed: %v", err)
		}
	}()

	log.Printf("VLESS API listening on %s and managing inbound %q", settings.APIListenAddress(), settings.InboundTag)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("HTTP server failed: %v", err)
		os.Exit(1)
	}
}
