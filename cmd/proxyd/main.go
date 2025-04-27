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

	// Create GCE API client
	api, err := gce.NewClient(cfg.CredentialsFile, cfg.ProjectID, cfg.DefaultZone)
	if err != nil {
		log.Fatalf("Failed to create GCE client: %v", err)
	}

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
