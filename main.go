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
	// A configured API address is only a claim. Probe it, and treat a failure
	// as "this node has no Xray API" -- otherwise every user change would try
	// a live apply, fail, and restart Xray to reconcile.
	liveAPI := CommandLiveAPI{Runtime: runtime, APIAddress: settings.XrayAPIAddress}
	liveEnabled := liveAPI.Available()
	if liveEnabled {
		if err := liveAPI.Probe(settings.InboundTag); err != nil {
			log.Printf("Xray API at %s did not answer (%v): falling back to restart-based user changes", settings.XrayAPIAddress, err)
			liveEnabled = false
		}
	}

	store := NewConfigStore(settings.XrayConfigFile, settings.InboundTag, settings.Flow, runtime)
	if liveEnabled {
		store = store.WithLiveAPI(liveAPI, settings.VLESSPort)
	}
	if settings.WsInboundTag != "" {
		store = store.WithWsInbound(settings.WsInboundTag, settings.WsPort)
	}
	if _, err := store.List(); err != nil {
		log.Fatalf("cannot load managed Xray users: %v", err)
	}

	api := NewAPIServer(settings, store, runtime)
	if liveEnabled {
		api = api.WithLiveAPI(liveAPI)
	}
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

	if liveEnabled {
		log.Printf("Xray gRPC API at %s: user changes apply without a restart", settings.XrayAPIAddress)
	} else {
		log.Printf("Xray gRPC API disabled: every user change restarts Xray and drops live sessions")
	}
	if settings.WsInboundTag != "" {
		log.Printf("Mirroring every user change into ws inbound %q as well", settings.WsInboundTag)
	}
	log.Printf("VLESS API listening on %s and managing inbound %q", settings.APIListenAddress(), settings.InboundTag)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("HTTP server failed: %v", err)
		os.Exit(1)
	}
}
