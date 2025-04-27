package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"time"

	"example.com/proxy/api"
)

// Clock interface for time operations
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
	NewTicker(d time.Duration) Ticker
	Since(t time.Time) time.Duration
}

type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// tickerWrapper wraps time.Ticker to implement our Ticker interface
type tickerWrapper struct {
	*time.Ticker
}

func (t tickerWrapper) C() <-chan time.Time {
	return t.Ticker.C
}

type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (systemClock) NewTicker(d time.Duration) Ticker       { return tickerWrapper{time.NewTicker(d)} }
func (systemClock) Since(t time.Time) time.Duration        { return time.Since(t) }

// SystemClock returns a real system clock implementation
func SystemClock() Clock { return systemClock{} }

// Config holds the proxy configuration
type Config struct {
	CredentialsFile     string `json:"credentials_file"`
	ProjectID           string `json:"project_id"`
	DefaultZone         string `json:"default_zone"`
	ListenPort          int    `json:"listen_port"`
	InstanceName        string `json:"instance_name"`
	InstancePort        int    `json:"instance_port"`
	IdleTimeoutSeconds  int    `json:"idle_timeout_seconds"`
	MonitorIntervalSecs int    `json:"monitor_interval_secs"`
	IdleShutdownMinutes int    `json:"idle_shutdown_minutes"`
}

// LoadConfig loads the proxy configuration from a JSON file
func LoadConfig(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	return &cfg, nil
}

// Server represents the proxy server
type Server struct {
	*http.Server
	cfg   *Config
	api   api.GCEAPI
	clock Clock
	mu    sync.RWMutex
	state *InstanceState
}

// InstanceState represents the state of the GCE instance
type InstanceState struct {
	IP        string
	Status    string
	LastUsed  time.Time
	StartTime time.Time
}

const (
	RetryHeaderName  = "X-Retry-After"
	RetryHeaderValue = "5"
)

// NewServer creates a new proxy server
func NewServer(cfg *Config, api api.GCEAPI, clock Clock) (*Server, error) {
	srv := &Server{
		cfg:   cfg,
		api:   api,
		clock: clock,
		state: &InstanceState{
			Status: "TERMINATED",
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.ServeHTTP)

	srv.Server = &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.ListenPort),
		Handler: mux,
	}

	return srv, nil
}

// ServeHTTP implements http.Handler
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if instance is running
	if s.state.Status != "RUNNING" {
		// Start instance if not running
		if err := s.startInstance(r.Context()); err != nil {
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
			return
		}
		// Set retry header for 503 responses
		w.Header().Set(RetryHeaderName, RetryHeaderValue)
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}

	// Update last used time
	s.state.LastUsed = s.clock.Now()

	// Create reverse proxy
	target := &url.URL{
		Scheme: "http",
		Host:   fmt.Sprintf("%s:%d", s.state.IP, s.cfg.InstancePort),
	}
	proxy := httputil.NewSingleHostReverseProxy(target)

	// Set forwarded headers
	r.Header.Set("X-Forwarded-Host", r.Host)
	r.Header.Set("X-Forwarded-Proto", r.Proto)
	r.Header.Set("X-Forwarded-For", r.RemoteAddr)

	proxy.ServeHTTP(w, r)
}

// startInstance starts the GCE instance
func (s *Server) startInstance(ctx context.Context) error {
	if s.state.Status == "STARTING" {
		return nil
	}

	s.state.Status = "STARTING"
	s.state.StartTime = s.clock.Now()

	go func() {
		if err := s.api.Start(ctx, s.cfg.ProjectID, s.cfg.DefaultZone, s.cfg.InstanceName); err != nil {
			s.mu.Lock()
			s.state.Status = "TERMINATED"
			s.mu.Unlock()
			return
		}

		// Get instance IP
		status, ip, err := s.api.Get(ctx, s.cfg.ProjectID, s.cfg.DefaultZone, s.cfg.InstanceName)
		if err != nil {
			s.mu.Lock()
			s.state.Status = "TERMINATED"
			s.mu.Unlock()
			return
		}

		s.mu.Lock()
		s.state.Status = status
		s.state.IP = ip
		s.mu.Unlock()

		// Start idle timeout goroutine
		go s.checkIdleTimeout()
	}()

	return nil
}

// checkIdleTimeout checks if the instance has been idle for too long
func (s *Server) checkIdleTimeout() {
	for {
		time.Sleep(time.Second)
		s.mu.RLock()
		if s.state.Status != "RUNNING" {
			s.mu.RUnlock()
			return
		}
		idle := s.clock.Since(s.state.LastUsed)
		s.mu.RUnlock()

		if idle > time.Duration(s.cfg.IdleTimeoutSeconds)*time.Second {
			s.mu.Lock()
			if s.state.Status == "RUNNING" {
				s.state.Status = "TERMINATED"
				s.api.Stop(context.Background(), s.cfg.ProjectID, s.cfg.DefaultZone, s.cfg.InstanceName)
			}
			s.mu.Unlock()
			return
		}
	}
}

// Shutdown gracefully shuts down the server
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.state.Status == "RUNNING" {
		s.api.Stop(ctx, s.cfg.ProjectID, s.cfg.DefaultZone, s.cfg.InstanceName)
	}
	s.mu.Unlock()
	return s.Server.Shutdown(ctx)
}

// ListenAndServe starts the HTTP server
func (s *Server) ListenAndServe() error {
	return s.Server.ListenAndServe()
}

// ListenAndServeTLS starts the HTTPS server
func (s *Server) ListenAndServeTLS(certFile, keyFile string) error {
	return s.Server.ListenAndServeTLS(certFile, keyFile)
}

// Close implements io.Closer
func (s *Server) Close() error {
	return s.Server.Close()
}
