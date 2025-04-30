package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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

	// Simulate the delay between RUNNING and ready to accept connections
	time.Sleep(100 * time.Millisecond)

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
		tick: make(chan time.Time, 100),
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
	return &fakeTicker{c: c.tick}
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

	// Send multiple ticks to ensure all goroutines get the update
	for i := 0; i < 30; i++ { // Increase from 20 to 30 for more reliability
		select {
		case c.tick <- c.now:
		default:
			// If channel is full, sleep briefly and try again
			time.Sleep(5 * time.Millisecond)
			select {
			case c.tick <- c.now:
			default:
				// If still can't send, that's ok, continue
			}
		}
	}

	// Wait longer to ensure all goroutines have processed the time change
	time.Sleep(100 * time.Millisecond)
}

type fakeTicker struct{ c chan time.Time }

func (t *fakeTicker) C() <-chan time.Time { return t.c }
func (t *fakeTicker) Stop()               {}

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

	// Create a mock clock for precise time control
	clock := newClock()

	// Create proxy server with test configuration
	cfg := &Config{
		CredentialsFile:     "test-credentials.json",
		ProjectID:           "test-project",
		DefaultZone:         "test-zone",
		ListenPort:          8080,
		InstanceName:        "test-instance",
		DestPort:            backend.Listener.Addr().(*net.TCPAddr).Port,
		IdleTimeoutSeconds:  300,
		MonitorIntervalSecs: 30,
		IdleShutdownMinutes: 15,
	}

	server, err := NewServer(cfg, api, clock)
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

	// Wait for instance to be ready
	if !api.waitForReady(1 * time.Second) {
		t.Fatal("Instance did not become ready in time")
	}

	// Advance clock past grace period
	clock.advance(StartupGracePeriod + 1*time.Second)
	time.Sleep(100 * time.Millisecond) // Give time for state to update

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
		t.Errorf("Expected response 'backend response', got '%s'", string(body))
	}
}

func TestStartupGracePeriod(t *testing.T) {
	// Create a test server to simulate the backend
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("backend response"))
	}))
	defer backend.Close()

	// Create a fake API that simulates GCE behavior
	api := newFakeAPI("RUNNING") // Start with instance already running
	api.backend = backend
	api.ip = "127.0.0.1" // Set IP address directly

	// Create a mock clock for precise time control
	clock := newClock()
	now := clock.Now()

	// Create proxy server with test configuration
	cfg := &Config{
		CredentialsFile:     "test-credentials.json",
		ProjectID:           "test-project",
		DefaultZone:         "test-zone",
		ListenPort:          8080,
		InstanceName:        "test-instance",
		DestPort:            backend.Listener.Addr().(*net.TCPAddr).Port,
		IdleTimeoutSeconds:  300,
		MonitorIntervalSecs: 30,
		IdleShutdownMinutes: 15,
	}

	server, err := NewServer(cfg, api, clock)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	// Explicitly set ReadyTime to test grace period - adding a small buffer to ensure it's exact
	readyTime := now.Add(60 * time.Second)
	server.mu.Lock()
	server.state.ReadyTime = readyTime // Set to exactly 60 seconds from now
	server.mu.Unlock()

	proxy := httptest.NewServer(server)
	defer proxy.Close()

	req, _ := http.NewRequest("GET", proxy.URL, nil)
	req.Host = "test.example.com"

	// Test 1: During grace period - should return 503
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("During grace period: Expected status 503, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Advance clock to exactly the end of grace period
	clock.advance(60 * time.Second)
	time.Sleep(250 * time.Millisecond) // Give more time for state to update

	// Test 2: At grace period end - should still return 503 due to comparison logic
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("At grace period end: Expected status 503, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Advance clock past grace period with a clear buffer
	clock.advance(2 * time.Second)     // Advance by 2 seconds instead of 1 for more reliable testing
	time.Sleep(250 * time.Millisecond) // Give more time for state to update

	// Test 3: After grace period - should return 200
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("After grace period: Expected status 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestWhitelistPaths(t *testing.T) {
	cfg := &Config{
		InstanceName: "vm",
		DestPort:     80,
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
	// Create a temporary test config file
	testConfig := `{
		"credentials_file": "test-credentials.json",
		"project_id": "test-project",
		"default_zone": "test-zone",
		"listen_port": 8080,
		"instance_name": "test-instance",
		"idle_timeout_seconds": 300,
		"dest_port": 80
	}`

	// Write the test config to a temporary file
	tmpFile, err := os.CreateTemp("", "test-config-*.json")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(testConfig); err != nil {
		t.Fatalf("Failed to write test config: %v", err)
	}
	if err := tmpFile.Close(); err != nil {
		t.Fatalf("Failed to close temp file: %v", err)
	}

	// Load the test config
	cfg, err := LoadConfig(tmpFile.Name())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	// Verify the loaded values
	if cfg.ProjectID != "test-project" {
		t.Errorf("expected project ID test-project, got %s", cfg.ProjectID)
	}

	if cfg.InstanceName != "test-instance" {
		t.Errorf("expected instance name test-instance, got %s", cfg.InstanceName)
	}

	if cfg.DestPort != 80 {
		t.Errorf("expected dest port 80, got %d", cfg.DestPort)
	}
}

func TestInstanceErrorHandling(t *testing.T) {
	cfg := &Config{
		InstanceName: "vm",
		DestPort:     80,
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
	if rec.Code != http.StatusBadGateway {
		t.Errorf("want status %d for missing IP, got %d", http.StatusBadGateway, rec.Code)
	}

	// Test invalid state
	api.mu.Lock()
	api.status = "INVALID_STATE"
	api.mu.Unlock()
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("want status %d for invalid state, got %d", http.StatusBadGateway, rec.Code)
	}
}

func TestCustomResponseHeaders(t *testing.T) {
	// Create a test server to simulate the backend
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("backend response"))
	}))
	defer backend.Close()

	// Create a fake API that simulates GCE behavior
	api := newFakeAPI("RUNNING") // Start with instance already running
	api.backend = backend
	api.ip = "127.0.0.1" // Set IP address directly

	// Create a mock clock for precise time control
	clock := newClock()

	// Create proxy server with test configuration
	cfg := &Config{
		CredentialsFile:     "test-credentials.json",
		ProjectID:           "test-project",
		DefaultZone:         "test-zone",
		ListenPort:          8080,
		InstanceName:        "test-instance",
		DestPort:            backend.Listener.Addr().(*net.TCPAddr).Port,
		IdleTimeoutSeconds:  300,
		MonitorIntervalSecs: 30,
		IdleShutdownMinutes: 15,
	}

	server, err := NewServer(cfg, api, clock)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	// Force ReadyTime to be in the past so the instance is immediately ready
	server.mu.Lock()
	server.state.ReadyTime = clock.Now().Add(-10 * time.Minute)
	server.mu.Unlock()

	proxy := httptest.NewServer(server)
	defer proxy.Close()

	// Make request to get response
	req, _ := http.NewRequest("GET", proxy.URL, nil)
	req.Host = "test.example.com"
	resp, err := http.DefaultClient.Do(req)
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
	api := newFakeAPI("RUNNING") // Start with instance already running
	api.backend = backend
	api.ip = "127.0.0.1" // Set IP address directly

	// Create a mock clock for precise time control
	clock := newClock()
	now := clock.Now()

	// Create proxy server with test configuration and short idle timeout
	cfg := &Config{
		CredentialsFile:     "test-credentials.json",
		ProjectID:           "test-project",
		DefaultZone:         "test-zone",
		ListenPort:          8080,
		InstanceName:        "test-instance",
		DestPort:            backend.Listener.Addr().(*net.TCPAddr).Port,
		IdleTimeoutSeconds:  1,
		MonitorIntervalSecs: 1,
		IdleShutdownMinutes: 1,
	}

	server, err := NewServer(cfg, api, clock)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	// Force ReadyTime to be in the past so the instance is immediately ready
	server.mu.Lock()
	server.state.ReadyTime = now.Add(-10 * time.Minute)
	server.state.LastUsed = now // Set LastUsed explicitly to now
	server.mu.Unlock()

	proxy := httptest.NewServer(server)
	defer proxy.Close()

	// First request should succeed immediately since instance is already running and ready
	req, _ := http.NewRequest("GET", proxy.URL, nil)
	req.Host = "test.example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Advance clock past idle timeout by a significant margin
	clock.advance(10 * time.Second)    // Well past the 1 second idle timeout
	time.Sleep(300 * time.Millisecond) // Give more time for state to update and stop operation to complete

	// Verify instance has been stopped
	api.mu.Lock()
	stopCount := api.stopN
	status := api.status
	api.mu.Unlock()

	if stopCount == 0 {
		t.Errorf("Expected instance to be stopped, but Stop() was not called")
	}

	if status != "TERMINATED" {
		t.Errorf("Expected instance status to be TERMINATED, got %s", status)
	}

	// Second request should get 503 as instance should be stopped by now
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("Expected status 503, got %d", resp.StatusCode)
	}
	resp.Body.Close()
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

	// Create a mock clock for precise time control
	clock := newClock()

	// Create proxy server with test configuration
	cfg := &Config{
		CredentialsFile:     "test-credentials.json",
		ProjectID:           "test-project",
		DefaultZone:         "test-zone",
		ListenPort:          8080,
		InstanceName:        "test-instance",
		DestPort:            backend.Listener.Addr().(*net.TCPAddr).Port,
		IdleTimeoutSeconds:  300,
		MonitorIntervalSecs: 30,
		IdleShutdownMinutes: 15,
	}

	server, err := NewServer(cfg, api, clock)
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

	// Advance clock past grace period
	clock.advance(StartupGracePeriod + 1*time.Second)
	time.Sleep(100 * time.Millisecond) // Give time for state to update

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
	api := newFakeAPI("RUNNING") // Start with instance already running
	api.backend = backend
	api.ip = "127.0.0.1" // Set IP address directly

	// Create a mock clock for precise time control
	clock := newClock()

	// Create proxy server with test configuration
	cfg := &Config{
		CredentialsFile:     "test-credentials.json",
		ProjectID:           "test-project",
		DefaultZone:         "test-zone",
		ListenPort:          8080,
		InstanceName:        "test-instance",
		DestPort:            backend.Listener.Addr().(*net.TCPAddr).Port,
		IdleTimeoutSeconds:  300,
		MonitorIntervalSecs: 30,
		IdleShutdownMinutes: 15,
	}

	server, err := NewServer(cfg, api, clock)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	// Force ReadyTime to be in the past so the instance is immediately ready
	server.mu.Lock()
	server.state.ReadyTime = clock.Now().Add(-10 * time.Minute)
	server.mu.Unlock()

	proxy := httptest.NewServer(server)
	defer proxy.Close()

	// Request should succeed immediately since instance is already running and ready
	req, _ := http.NewRequest("GET", proxy.URL, nil)
	req.Host = "test.example.com"
	resp, err := http.DefaultClient.Do(req)
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

func TestHealthEndpoint(t *testing.T) {
	// Create a fake API that simulates GCE behavior
	api := newFakeAPI("RUNNING") // Start with instance already running
	api.ip = "192.168.1.1"       // Set IP address directly

	// Create a mock clock for precise time control
	clock := newClock()
	now := clock.Now()

	// Create proxy server with test configuration
	cfg := &Config{
		CredentialsFile:    "test-credentials.json",
		ProjectID:          "test-project",
		DefaultZone:        "test-zone",
		ListenPort:         8080,
		InstanceName:       "test-instance",
		DestPort:           80,
		IdleTimeoutSeconds: 300,
	}

	server, err := NewServer(cfg, api, clock)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	// Set specific times for testing
	server.mu.Lock()
	server.state.LastUsed = now.Add(-10 * time.Minute)
	server.state.StartTime = now.Add(-20 * time.Minute)
	server.mu.Unlock()

	// Create a test server with our health handler
	proxy := httptest.NewServer(http.HandlerFunc(server.handleHealth))
	defer proxy.Close()

	// Make request to health endpoint
	resp, err := http.Get(proxy.URL)
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	defer resp.Body.Close()

	// Check status code
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	// Check content type
	if contentType := resp.Header.Get("Content-Type"); contentType != "application/json" {
		t.Errorf("Expected Content-Type: application/json, got %s", contentType)
	}

	// Parse response
	var healthData struct {
		Status    string    `json:"status"`
		IP        string    `json:"ip"`
		LastUsed  time.Time `json:"last_used"`
		StartTime time.Time `json:"start_time"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&healthData); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	// Verify response data
	if healthData.Status != "RUNNING" {
		t.Errorf("Expected status RUNNING, got %s", healthData.Status)
	}
	if healthData.IP != "192.168.1.1" {
		t.Errorf("Expected IP 192.168.1.1, got %s", healthData.IP)
	}
	if !healthData.LastUsed.Equal(server.state.LastUsed) {
		t.Errorf("Expected LastUsed %v, got %v", server.state.LastUsed, healthData.LastUsed)
	}
	if !healthData.StartTime.Equal(server.state.StartTime) {
		t.Errorf("Expected StartTime %v, got %v", server.state.StartTime, healthData.StartTime)
	}
}
