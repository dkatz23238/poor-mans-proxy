package gce

import (
	"context"
	"fmt"
	"log"

	compute "cloud.google.com/go/compute/apiv1"
	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"example.com/proxy/api"
	"google.golang.org/api/option"
)

// Client implements the api.GCEAPI interface for Google Compute Engine
type Client struct {
	client      *compute.InstancesClient
	projectID   string
	defaultZone string
}

// NewClient creates a new GCE API client
func NewClient(credentialsFile, projectID, defaultZone string) (api.GCEAPI, error) {
	log.Printf("Initializing GCE client with credentials from %s", credentialsFile)
	ctx := context.Background()
	client, err := compute.NewInstancesRESTClient(ctx, option.WithCredentialsFile(credentialsFile))
	if err != nil {
		return nil, fmt.Errorf("creating GCE client: %w", err)
	}
	log.Printf("Successfully initialized GCE client for project %s in zone %s", projectID, defaultZone)

	return &Client{
		client:      client,
		projectID:   projectID,
		defaultZone: defaultZone,
	}, nil
}

// Get returns the status and IP of an instance
func (g *Client) Get(ctx context.Context, project, zone, instanceName string) (string, string, error) {
	if project == "" {
		project = g.projectID
	}
	if zone == "" {
		zone = g.defaultZone
	}

	instance, err := g.client.Get(ctx, &computepb.GetInstanceRequest{
		Instance: instanceName,
		Project:  project,
		Zone:     zone,
	})
	if err != nil {
		return "", "", fmt.Errorf("getting instance: %w", err)
	}

	status := ""
	if instance.Status != nil {
		status = *instance.Status
	}

	ip := ""
	if len(instance.NetworkInterfaces) > 0 &&
		len(instance.NetworkInterfaces[0].AccessConfigs) > 0 &&
		instance.NetworkInterfaces[0].AccessConfigs[0].NatIP != nil {
		ip = *instance.NetworkInterfaces[0].AccessConfigs[0].NatIP
	}

	return status, ip, nil
}

// Start starts an instance
func (g *Client) Start(ctx context.Context, project, zone, instanceName string) error {
	if project == "" {
		project = g.projectID
	}
	if zone == "" {
		zone = g.defaultZone
	}

	op, err := g.client.Start(ctx, &computepb.StartInstanceRequest{
		Instance: instanceName,
		Project:  project,
		Zone:     zone,
	})
	if err != nil {
		return fmt.Errorf("starting instance: %w", err)
	}

	return op.Wait(ctx)
}

// Stop stops an instance
func (g *Client) Stop(ctx context.Context, project, zone, instanceName string) error {
	if project == "" {
		project = g.projectID
	}
	if zone == "" {
		zone = g.defaultZone
	}

	op, err := g.client.Stop(ctx, &computepb.StopInstanceRequest{
		Instance: instanceName,
		Project:  project,
		Zone:     zone,
	})
	if err != nil {
		return fmt.Errorf("stopping instance: %w", err)
	}

	return op.Wait(ctx)
}
