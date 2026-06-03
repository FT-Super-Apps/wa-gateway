package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"wa-gateway/internal/api"
	"wa-gateway/internal/config"
	"wa-gateway/internal/gateway"

	"github.com/joho/godotenv"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func main() {
	// Muat .env jika ada (diabaikan jika tidak ditemukan)
	_ = godotenv.Load()

	cfg := config.Load()
	log := waLog.Stdout("Main", cfg.LogLevel, true)

	gw, err := gateway.NewManager(cfg)
	if err != nil {
		log.Errorf("failed to init gateway: %v", err)
		os.Exit(1)
	}

	if err := gw.Start(context.Background()); err != nil {
		log.Errorf("failed to start gateway: %v", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           api.New(cfg, gw).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Infof("REST API listening on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Errorf("http server error: %v", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Infof("Shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	gw.Stop()
}
