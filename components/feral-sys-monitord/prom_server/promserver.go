package prom_server

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

type PromServer interface {
	// Start starts the Prometheus server
	Start() error
	// Stop stops the Prometheus server
	Stop() error
}

type promServer struct {
	server *http.Server
	logger *zap.Logger
}

func New(logger *zap.Logger) PromServer {
	// Unregister default Go collectors to have clean metrics
	prometheus.DefaultRegisterer.Unregister(collectors.NewGoCollector())
	prometheus.DefaultRegisterer.Unregister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	return &promServer{
		logger: logger,
	}
}

func (s *promServer) Start() error {
	s.server = &http.Server{
		Addr:              "localhost:9001",
		Handler:           promhttp.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Error("Prometheus server failed", zap.Error(err))
		}
	}()

	s.logger.Info("Prometheus server started on localhost:9001")
	return nil
}

func (s *promServer) Stop() error {
	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := s.server.Shutdown(ctx)
		s.logger.Info("Prometheus server stopped")
		return err
	}
	return nil
}
