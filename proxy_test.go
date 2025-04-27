package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/proxy/api"
)

// ---- simple fakes -----------------------------------------------------------

type fakeAPI struct {
	status string
	ip     string
	startN int
	stopN  int
	mu     sync.Mutex
	// Channel to signal when instance is ready
	ready chan struct{}
	// Channel to signal when instance is starting
	starting chan struct{}
	backend  *httptest.Server
}

var _ api.GCEAPI = (*fakeAPI)(nil) // Verify fakeAPI implements api.GCEAPI

func newFakeAPI(initialStatus string) *fakeAPI {
	return &fakeAPI{
		status:   initialStatus,
		ready:    make(chan struct{}, 1),
		starting: make(chan struct{}, 1),
	}
}

func (f *fakeAPI) Get(ctx context.Context, project, zone, instanceName string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// If instance is running and we have a backend server, use its address as the IP
	if f.status == "RUNNING" && f.backend != nil {
		addr := f.backend.Listener.Addr().String()
		// Extract just the IP part if it's in format "127.0.0.1:port"
		if idx := strings.LastIndex(addr, ":"); idx > 0 {
			f.ip = addr[:idx]
		} else {
			f.ip = addr
		}
	}

	return f.status, f.ip, nil
}

func (f *fakeAPI) Start(ctx context.Context, project, zone, instanceName string) error {
	// Signal that we're starting
	select {
	case f.starting <- struct{}{}:
	default:
	}

	f.mu.Lock()
	f.startN++
	f.status = "PROVISIONING" // Set intermediate state
	f.mu.Unlock()

	// Simulate GCE instance startup delay
	time.Sleep(50 * time.Millisecond)

	f.mu.Lock()
	f.status = "RUNNING"
	// Set IP to backend server address if available
	if f.backend != nil {
		addr := f.backend.Listener.Addr().String()
		// Extract just the IP part if it's in format "127.0.0.1:port"
		if idx := strings.LastIndex(addr, ":"); idx > 0 {
			f.ip = addr[:idx]
		} else {
			f.ip = addr
		}
	} else {
		f.ip = "127.0.0.1"
	}
	f.mu.Unlock()

	// Signal that instance is ready
	select {
	case f.ready <- struct{}{}:
	default:
	}

	return nil
}

func (f *fakeAPI) Stop(ctx context.Context, project, zone, instanceName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopN++
	f.status = "TERMINATED"
	f.ip = ""
	return nil
}

// waitForStarting waits for the instance to start starting
func (f *fakeAPI) waitForStarting(timeout time.Duration) bool {
	select {
	case <-f.starting:
		return true
	case <-time.After(timeout):
		return false
	}
}

// waitForReady waits for the instance to be ready
func (f *fakeAPI) waitForReady(timeout time.Duration) bool {
	select {
	case <-f.ready:
		return true
	case <-time.After(timeout):
		return false
	}
}

type mockClock struct {
	now  time.Time
	tick chan time.Time
	mu   sync.Mutex
}

func newClock() *mockClock {
	return &mockClock{
		now:  time.Now(),
		tick: make(chan time.Time, 10),
	}
}

func (c *mockClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *mockClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- c.now
	return ch
}

func (c *mockClock) NewTicker(d time.Duration) Ticker {
	return fakeTicker{c.tick}
}

func (c *mockClock) Since(t time.Time) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now.Sub(t)
}

func (c *mockClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	c.tick <- c.now
}

type fakeTicker struct{ c chan time.Time }

func (t fakeTicker) C() <-chan time.Time { return t.c }
func (t fakeTicker) Stop()               {}

// ---- test -------------------------------------------------------------------

func TestStartThenProxy(t *testing.T) {
	// Create a test server to simulate the backend
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("backend response"))
	}))
	defer backend.Close()

	// Create a fake API that simulates GCE behavior
	api := newFakeAPI("TERMINATED")
	api.backend = backend

	// Create proxy server with test configuration
	cfg := &Config{
		CredentialsFile:     "test-credentials.json",
		ProjectID:           "test-project",
		DefaultZone:         "test-zone",
		ListenPort:          8080,
		InstanceName:        "test-instance",
		InstancePort:        backend.Listener.Addr().(*net.TCPAddr).Port, // Use the actual backend port
		IdleTimeoutSeconds:  300,
		MonitorIntervalSecs: 30,
		IdleShutdownMinutes: 15,
	}

	server, err := NewServer(cfg, api, SystemClock())
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	proxy := httptest.NewServer(server)
	defer proxy.Close()

	// First request should trigger instance start and return 503
	req, _ := http.NewRequest("GET", proxy.URL, nil)
	req.Host = "test.example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("Expected status 503, got %d", resp.StatusCode)
	}
	if resp.Header.Get(RetryHeaderName) != RetryHeaderValue {
		t.Errorf("Expected retry header %s, got %s", RetryHeaderValue, resp.Header.Get(RetryHeaderName))
	}

	// Second request should also return 503 while instance is starting
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("Expected status 503, got %d", resp.StatusCode)
	}

	// Wait for instance to be ready
	if !api.waitForReady(1 * time.Second) {
		t.Fatal("Instance did not become ready in time")
	}

	// Third request should succeed and proxy to backend
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "backend response" {
		t.Errorf("Expected body 'backend response', got %s", string(body))
	}
}

func TestWhitelistPaths(t *testing.T) {
	cfg := &Config{
		InstanceName: "vm",
		InstancePort: 80,
		ProjectID:    "test-project",
		DefaultZone:  "test-zone",
	}

	api := newFakeAPI("TERMINATED")
	srv, _ := NewServer(cfg, api, SystemClock())

	tests := []struct {
		path       string
		wantStatus int
	}{
		{"/api/users", http.StatusServiceUnavailable}, // Should try to start
		{"/health", http.StatusServiceUnavailable},    // Should try to start
		{"/other", http.StatusServiceUnavailable},     // Should try to start
		{"/api", http.StatusServiceUnavailable},       // Should try to start
	}

	for _, tc := range tests {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "http://any-host"+tc.path, nil)
		srv.ServeHTTP(rec, req)
		if rec.Code != tc.wantStatus {
			t.Errorf("path %q: want status %d, got %d", tc.path, tc.wantStatus, rec.Code)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	cfg, err := LoadConfig("config.json")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.ProjectID != "your-project-id" {
		t.Errorf("expected project ID your-project-id, got %s", cfg.ProjectID)
	}

	if cfg.InstanceName != "sigma-server" {
		t.Errorf("expected instance name sigma-server, got %s", cfg.InstanceName)
	}

	if cfg.InstancePort != 0 {
		t.Errorf("expected instance port 0, got %d", cfg.InstancePort)
	}
}

func TestInstanceErrorHandling(t *testing.T) {
	cfg := &Config{
		InstanceName: "vm",
		InstancePort: 80,
		ProjectID:    "test-project",
		DefaultZone:  "test-zone",
	}

	api := newFakeAPI("RUNNING")
	api.ip = "" // Simulate missing IP
	srv, _ := NewServer(cfg, api, SystemClock())

	// Test missing IP
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://any-host/", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("want status %d for missing IP, got %d", http.StatusServiceUnavailable, rec.Code)
	}

	// Test invalid state
	api.mu.Lock()
	api.status = "INVALID_STATE"
	api.mu.Unlock()
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("want status %d for invalid state, got %d", http.StatusServiceUnavailable, rec.Code)
	}
}

func TestCustomResponseHeaders(t *testing.T) {
	// Create a test server to simulate the backend
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("backend response"))
	}))
	defer backend.Close()

	// Create a fake API that simulates GCE behavior
	api := newFakeAPI("TERMINATED")
	api.backend = backend

	// Create proxy server with test configuration
	cfg := &Config{
		CredentialsFile:     "test-credentials.json",
		ProjectID:           "test-project",
		DefaultZone:         "test-zone",
		ListenPort:          8080,
		InstanceName:        "test-instance",
		InstancePort:        backend.Listener.Addr().(*net.TCPAddr).Port, // Use the actual backend port
		IdleTimeoutSeconds:  300,
		MonitorIntervalSecs: 30,
		IdleShutdownMinutes: 15,
	}

	server, err := NewServer(cfg, api, SystemClock())
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	proxy := httptest.NewServer(server)
	defer proxy.Close()

	// Make request to trigger instance start
	req, _ := http.NewRequest("GET", proxy.URL, nil)
	req.Host = "test.example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}

	// Wait for instance to be ready
	if !api.waitForReady(1 * time.Second) {
		t.Fatal("Instance did not become ready in time")
	}

	// Make request to get response
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}
}

func TestIdleTimeout(t *testing.T) {
	// Create a test server to simulate the backend
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("backend response"))
	}))
	defer backend.Close()

	// Create a fake API that simulates GCE behavior
	api := newFakeAPI("TERMINATED")
	api.backend = backend

	// Create proxy server with test configuration and short idle timeout
	cfg := &Config{
		CredentialsFile:     "test-credentials.json",
		ProjectID:           "test-project",
		DefaultZone:         "test-zone",
		ListenPort:          8080,
		InstanceName:        "test-instance",
		InstancePort:        backend.Listener.Addr().(*net.TCPAddr).Port, // Use the actual backend port
		IdleTimeoutSeconds:  1,                                           // Short timeout for testing
		MonitorIntervalSecs: 1,
		IdleShutdownMinutes: 1,
	}

	server, err := NewServer(cfg, api, SystemClock())
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	proxy := httptest.NewServer(server)
	defer proxy.Close()

	// Make request to trigger instance start
	req, _ := http.NewRequest("GET", proxy.URL, nil)
	req.Host = "test.example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}

	// Wait for instance to be ready
	if !api.waitForReady(1 * time.Second) {
		t.Fatal("Instance did not become ready in time")
	}

	// Make request to get response
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	// Wait for idle timeout
	time.Sleep(2 * time.Second)

	// Make another request to verify instance was stopped
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("Expected status 503, got %d", resp.StatusCode)
	}
}

func TestForwardedHeaders(t *testing.T) {
	// Create a test server to simulate the backend
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check forwarded headers
		if r.Header.Get("X-Forwarded-Host") != "test.example.com" {
			t.Errorf("Expected X-Forwarded-Host: test.example.com, got %s", r.Header.Get("X-Forwarded-Host"))
		}
		if r.Header.Get("X-Forwarded-Proto") != "HTTP/1.1" {
			t.Errorf("Expected X-Forwarded-Proto: HTTP/1.1, got %s", r.Header.Get("X-Forwarded-Proto"))
		}
		if r.Header.Get("X-Forwarded-For") == "" {
			t.Error("Expected X-Forwarded-For to be set")
		}
		w.Write([]byte("backend response"))
	}))
	defer backend.Close()

	// Create a fake API that simulates GCE behavior
	api := newFakeAPI("TERMINATED")
	api.backend = backend

	// Create proxy server with test configuration
	cfg := &Config{
		CredentialsFile:     "test-credentials.json",
		ProjectID:           "test-project",
		DefaultZone:         "test-zone",
		ListenPort:          8080,
		InstanceName:        "test-instance",
		InstancePort:        backend.Listener.Addr().(*net.TCPAddr).Port, // Use the actual backend port
		IdleTimeoutSeconds:  300,
		MonitorIntervalSecs: 30,
		IdleShutdownMinutes: 15,
	}

	server, err := NewServer(cfg, api, SystemClock())
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	proxy := httptest.NewServer(server)
	defer proxy.Close()

	// Make request to trigger instance start
	req, _ := http.NewRequest("GET", proxy.URL, nil)
	req.Host = "test.example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}

	// Wait for instance to be ready
	if !api.waitForReady(1 * time.Second) {
		t.Fatal("Instance did not become ready in time")
	}

	// Make request to get response with forwarded headers
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}
}

func TestProtocolHandling(t *testing.T) {
	// Create a test server to simulate the backend
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("backend response"))
	}))
	defer backend.Close()

	// Create a fake API that simulates GCE behavior
	api := newFakeAPI("TERMINATED")
	api.backend = backend

	// Create proxy server with test configuration
	cfg := &Config{
		CredentialsFile:     "test-credentials.json",
		ProjectID:           "test-project",
		DefaultZone:         "test-zone",
		ListenPort:          8080,
		InstanceName:        "test-instance",
		InstancePort:        backend.Listener.Addr().(*net.TCPAddr).Port,
		IdleTimeoutSeconds:  300,
		MonitorIntervalSecs: 30,
		IdleShutdownMinutes: 15,
	}

	server, err := NewServer(cfg, api, SystemClock())
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	proxy := httptest.NewServer(server)
	defer proxy.Close()

	// First request should trigger instance start and return 503
	req, _ := http.NewRequest("GET", proxy.URL, nil)
	req.Host = "test.example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("Expected status 503, got %d", resp.StatusCode)
	}

	// Wait for instance to be ready
	if !api.waitForReady(1 * time.Second) {
		t.Fatal("Instance did not become ready in time")
	}

	// Second request should succeed and proxy to backend
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "backend response" {
		t.Errorf("Expected body 'backend response', got %s", string(body))
	}
}
