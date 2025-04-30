package main

import (
	"context"
	"flag"
	"log"

	compute "cloud.google.com/go/compute/apiv1"
	"example.com/proxy"
)

func main() {
	// Define command line flags
	action := flag.String("action", "", "Action to perform (start/stop)")
	configFile := flag.String("config", "config.json", "Path to config file")
	flag.Parse()

	// Validate required flags
	if *action == "" {
		log.Fatal("Missing required flag. Usage: instance -action=start|stop [-config=CONFIG_FILE]")
	}

	// Load configuration
	cfg, err := proxy.LoadConfig(*configFile)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Create GCE client
	ctx := context.Background()
	client, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		log.Fatalf("Failed to create GCE client: %v", err)
	}
	defer client.Close()

	// Create GCE API
	gceAPI := proxy.NewGCEAPI(client, cfg.ProjectID, cfg.DefaultZone)

	// Perform the requested action
	switch *action {
	case "start":
		if err := gceAPI.Start(ctx, cfg.ProjectID, cfg.DefaultZone, cfg.InstanceName); err != nil {
			log.Fatalf("Failed to start instance: %v", err)
		}
		log.Printf("Successfully started instance %s", cfg.InstanceName)
	case "stop":
		if err := gceAPI.Stop(ctx, cfg.ProjectID, cfg.DefaultZone, cfg.InstanceName); err != nil {
			log.Fatalf("Failed to stop instance: %v", err)
		}
		log.Printf("Successfully stopped instance %s", cfg.InstanceName)
	default:
		log.Fatal("Invalid action. Use 'start' or 'stop'")
	}
}
