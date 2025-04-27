package api

import "context"

// GCEAPI defines the interface for GCE operations
type GCEAPI interface {
	Get(ctx context.Context, project, zone, instanceName string) (string, string, error)
	Start(ctx context.Context, project, zone, instanceName string) error
	Stop(ctx context.Context, project, zone, instanceName string) error
}
