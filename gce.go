package proxy

import (
	"context"
	"fmt"

	compute "cloud.google.com/go/compute/apiv1"
	computepb "cloud.google.com/go/compute/apiv1/computepb"
)

// GCEAPI implements the API interface for Google Compute Engine
type GCEAPI struct {
	client      *compute.InstancesClient
	projectID   string
	defaultZone string
}

// NewGCEAPI creates a new GCE API client
func NewGCEAPI(client *compute.InstancesClient, projectID, defaultZone string) *GCEAPI {
	return &GCEAPI{
		client:      client,
		projectID:   projectID,
		defaultZone: defaultZone,
	}
}

// Get returns the status and IP of an instance
func (g *GCEAPI) Get(ctx context.Context, project, zone, instanceName string) (string, string, error) {
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
func (g *GCEAPI) Start(ctx context.Context, project, zone, instanceName string) error {
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
func (g *GCEAPI) Stop(ctx context.Context, project, zone, instanceName string) error {
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