package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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
	DestPort            int    `json:"dest_port"`
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
	// ReadyTime tracks when the instance will be ready to accept traffic
	ReadyTime time.Time
}

const (
	RetryHeaderName  = "X-Retry-After"
	RetryHeaderValue = "5"
	// StartupGracePeriod is the time to wait after instance start before accepting traffic
	StartupGracePeriod = 60 * time.Second
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

	// Check initial instance state
	status, ip, err := api.Get(context.Background(), cfg.ProjectID, cfg.DefaultZone, cfg.InstanceName)
	if err != nil {
		log.Printf("Failed to get initial state of instance %s: %v", cfg.InstanceName, err)
	} else {
		log.Printf("Initial state of instance %s: %s (IP: %s)", cfg.InstanceName, status, ip)
		srv.state.Status = status
		srv.state.IP = ip
		srv.state.LastUsed = clock.Now()

		// If instance is already running, start idle timeout check
		if status == "RUNNING" {
			log.Printf("Instance is already running, starting idle timeout check")
			go srv.checkIdleTimeout()
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.ServeHTTP)
	mux.HandleFunc("/health", srv.handleHealth)

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

	log.Printf("Received request for %s, instance state: %s", r.URL.Path, s.state.Status)

	// If instance is not running, start it
	if s.state.Status != "RUNNING" {
		log.Printf("Instance not running (status: %s), attempting to start", s.state.Status)
		if err := s.startInstance(r.Context()); err != nil {
			log.Printf("Failed to start instance: %v", err)
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set(RetryHeaderName, RetryHeaderValue)
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}

	// If instance is running but not ready (within grace period), return 503
	// Use strictly greater than comparison for more predictable test behavior
	if !s.clock.Now().After(s.state.ReadyTime) {
		log.Printf("Instance not ready yet (ready at: %v)", s.state.ReadyTime)
		w.Header().Set(RetryHeaderName, RetryHeaderValue)
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}

	log.Printf("Instance is running and ready (IP: %s), forwarding request", s.state.IP)

	// Update last used time
	s.state.LastUsed = s.clock.Now()

	// Create reverse proxy
	target := &url.URL{
		Scheme: "http",
		Host:   fmt.Sprintf("%s:%d", s.state.IP, s.cfg.DestPort),
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
	// If instance is already starting, don't try to start it again
	if s.state.Status == "STARTING" {
		log.Printf("Instance already starting, waiting for it to become RUNNING")
		return nil
	}

	// If instance is already running, don't try to start it again
	if s.state.Status == "RUNNING" {
		log.Printf("Instance already running, waiting for it to become ready")
		// If ReadyTime is not set, set it to now + grace period
		if s.state.ReadyTime.IsZero() {
			s.state.ReadyTime = s.clock.Now().Add(StartupGracePeriod)
			log.Printf("Instance %s is running but not ready yet (ready at: %v)",
				s.cfg.InstanceName, s.state.ReadyTime)
		}
		return nil
	}

	log.Printf("Starting instance %s (current state: %s)", s.cfg.InstanceName, s.state.Status)
	s.state.Status = "STARTING"
	s.state.StartTime = s.clock.Now()

	go func() {
		log.Printf("Initiating instance start operation for %s", s.cfg.InstanceName)
		// Use background context for the long-running operation
		bgCtx := context.Background()
		if err := s.api.Start(bgCtx, s.cfg.ProjectID, s.cfg.DefaultZone, s.cfg.InstanceName); err != nil {
			log.Printf("Failed to start instance %s: %v", s.cfg.InstanceName, err)
			s.mu.Lock()
			s.state.Status = "TERMINATED"
			s.mu.Unlock()
			return
		}

		log.Printf("Instance start operation completed, checking status and IP")
		// Get instance IP
		status, ip, err := s.api.Get(bgCtx, s.cfg.ProjectID, s.cfg.DefaultZone, s.cfg.InstanceName)
		if err != nil {
			log.Printf("Failed to get instance %s status: %v", s.cfg.InstanceName, err)
			s.mu.Lock()
			s.state.Status = "TERMINATED"
			s.mu.Unlock()
			return
		}

		s.mu.Lock()
		oldStatus := s.state.Status
		s.state.Status = status
		s.state.IP = ip
		// Set the ready time to be 60 seconds after the instance becomes RUNNING
		if status == "RUNNING" {
			s.state.ReadyTime = s.clock.Now().Add(StartupGracePeriod)
			log.Printf("Instance %s is running but not ready yet (ready at: %v)",
				s.cfg.InstanceName, s.state.ReadyTime)
		}
		// Reset last used time when instance starts
		s.state.LastUsed = s.clock.Now()
		s.mu.Unlock()

		log.Printf("Instance %s state changed: %s -> %s (IP: %s, ready at: %v)",
			s.cfg.InstanceName, oldStatus, status, ip, s.state.ReadyTime)

		// Start idle timeout goroutine
		go s.checkIdleTimeout()
	}()

	return nil
}

// checkIdleTimeout checks if the instance has been idle for too long
func (s *Server) checkIdleTimeout() {
	ticker := s.clock.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C():
			s.mu.RLock()
			status := s.state.Status
			ready := s.clock.Now().After(s.state.ReadyTime)
			lastUsed := s.state.LastUsed
			s.mu.RUnlock()

			// Exit if instance is not running
			if status != "RUNNING" {
				log.Printf("Instance not running (status: %s), stopping idle timeout check", status)
				return
			}

			// Only check idle time if we're past the startup grace period
			if !ready {
				continue
			}

			// Calculate idle time outside the read lock
			idle := s.clock.Since(lastUsed)
			idleTimeout := time.Duration(s.cfg.IdleTimeoutSeconds) * time.Second

			log.Printf("Instance idle time: %v (timeout: %v)", idle, idleTimeout)
			if idle > idleTimeout {
				s.mu.Lock()
				// Double-check status after acquiring write lock to avoid race conditions
				if s.state.Status == "RUNNING" {
					log.Printf("Instance %s idle for %v, shutting down",
						s.cfg.InstanceName, idle)
					s.state.Status = "TERMINATED"
					s.mu.Unlock()

					// Stop the instance without holding the lock to avoid deadlocks
					if err := s.api.Stop(context.Background(), s.cfg.ProjectID, s.cfg.DefaultZone, s.cfg.InstanceName); err != nil {
						log.Printf("Failed to stop instance %s: %v", s.cfg.InstanceName, err)
					} else {
						log.Printf("Successfully stopped instance %s", s.cfg.InstanceName)
					}
				} else {
					s.mu.Unlock()
				}
				return
			}
		}
	}
}

// Shutdown gracefully shuts down the server
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Just log the instance state, don't stop it
	if s.state.Status == "RUNNING" {
		log.Printf("Proxy shutting down, instance %s is still running (IP: %s)",
			s.cfg.InstanceName, s.state.IP)
	}

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

// handleHealth implements the health check endpoint
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	resp := struct {
		Status    string    `json:"status"`
		IP        string    `json:"ip,omitempty"`
		LastUsed  time.Time `json:"last_used,omitempty"`
		StartTime time.Time `json:"start_time,omitempty"`
	}{
		Status:    s.state.Status,
		IP:        s.state.IP,
		LastUsed:  s.state.LastUsed,
		StartTime: s.state.StartTime,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("Failed to encode health response: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
}
