package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/config"
	"github.com/auucoder/gptgrok2api-go/internal/httpapi"
)

func main() {
	cfg, err := config.Load("")
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	logFile, logErr := os.OpenFile(filepath.Join(cfg.RootDir, "logs", "app.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if logErr == nil {
		defer logFile.Close()
		log.SetOutput(io.MultiWriter(os.Stdout, logFile))
	} else {
		log.Printf("open runtime log: %v", logErr)
	}

	server := &http.Server{
		Addr:                         cfg.ListenAddr,
		Handler:                      httpapi.New(cfg).Handler(),
		ReadHeaderTimeout:            10 * time.Second,
		ReadTimeout:                  cfg.RequestTimeout,
		WriteTimeout:                 0,
		IdleTimeout:                  120 * time.Second,
		MaxHeaderBytes:               1 << 20,
		DisableGeneralOptionsHandler: false,
		// Streaming endpoints deliberately keep WriteTimeout disabled.
	}

	go func() {
		log.Printf("gptgrok2api-go listening on %s", cfg.ListenAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
