package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Shbhom/llm-proxy/internal/config"
	store "github.com/Shbhom/llm-proxy/internal/db"
	"github.com/Shbhom/llm-proxy/internal/handler"
	"github.com/Shbhom/llm-proxy/internal/metrics"
	"github.com/Shbhom/llm-proxy/internal/pool"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	st, err := store.OpenPath(cfg.Persistence.DSN)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	pl, err := pool.New(cfg, st)
	if err != nil {
		log.Fatalf("pool: %v", err)
	}
	defer pl.Stop()

	h, err := handler.NewOpenAIHandler(cfg, pl, st)
	if err != nil {
		log.Fatalf("handler: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	if cfg.Server.MetricsAddr != "" {
		go func() {
			log.Printf("metrics listening on %s", cfg.Server.MetricsAddr)
			if err := http.ListenAndServe(cfg.Server.MetricsAddr, metrics.Handler()); err != nil && err != http.ErrServerClosed {
				log.Printf("metrics server error: %v", err)
			}
		}()
	}

	// Mount our OpenAI-compatible endpoints
	mux.Handle("/openai/v1/", h)

	srv := &http.Server{
		Addr:              cfg.Server.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		log.Printf("proxy listening on %s → %s/openai/v1", cfg.Server.ListenAddr, cfg.Upstream.BaseURL)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
