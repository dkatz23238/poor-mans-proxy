package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"example.com/proxy"
	"example.com/proxy/gce"
)

func main() {
	configFile := flag.String("config", "config.json", "path to config file")
	flag.Parse()

	// Load configuration
	cfg, err := proxy.LoadConfig(*configFile)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Verify credentials file exists
	if _, err := os.Stat(cfg.CredentialsFile); err != nil {
		log.Fatalf("Credentials file %s does not exist or is not accessible: %v", cfg.CredentialsFile, err)
	}
	log.Printf("Using credentials file: %s", cfg.CredentialsFile)

	// Create GCE API client
	log.Printf("Creating GCE client for project %s in zone %s", cfg.ProjectID, cfg.DefaultZone)
	api, err := gce.NewClient(cfg.CredentialsFile, cfg.ProjectID, cfg.DefaultZone)
	if err != nil {
		log.Fatalf("Failed to create GCE client: %v", err)
	}
	log.Printf("Successfully created GCE client")

	// Create proxy server
	srv, err := proxy.NewServer(cfg, api, proxy.SystemClock())
	if err != nil {
		log.Fatalf("Failed to create proxy server: %v", err)
	}

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Shutting down...")
		if err := srv.Shutdown(context.Background()); err != nil {
			log.Printf("Error during shutdown: %v", err)
		}
	}()

	// Start server
	log.Printf("Proxy listening on port %d", cfg.ListenPort)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
