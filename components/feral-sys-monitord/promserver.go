package main

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

type PromServer struct {
	server *http.Server
	logger *zap.Logger
}

func NewPromServer(logger *zap.Logger) *PromServer {
	// Unregister default Go collectors to have clean metrics
	prometheus.DefaultRegisterer.Unregister(collectors.NewGoCollector())
	prometheus.DefaultRegisterer.Unregister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	return &PromServer{
		logger: logger,
	}
}

func (s *PromServer) Start() error {
	s.server = &http.Server{
		Addr:    "localhost:9001",
		Handler: promhttp.Handler(),
	}

	go func() {
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Error("Prometheus server failed", zap.Error(err))
		}
	}()

	s.logger.Info("Prometheus server started on localhost:9001")
	return nil
}

func (s *PromServer) Stop() error {
	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := s.server.Shutdown(ctx)
		s.logger.Info("Prometheus server stopped")
		return err
	}
	return nil
}
