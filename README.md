# Poor Man's Proxy

A simple HTTP proxy that automatically starts and stops a GCE instance based on demand.

## Features

- Automatically starts a GCE instance when a request is received
- Automatically stops the instance after a configurable idle timeout
- Supports graceful shutdown of the instance when the proxy is stopped
- Comprehensive logging of instance lifecycle events
- Simple configuration via JSON file
- Health check endpoint for monitoring

## Configuration

Create a `config.json` file with the following structure:

```json
{
    "credentials_file": "path/to/credentials.json",
    "project_id": "your-project-id",
    "default_zone": "us-central1-a",
    "listen_port": 8080,
    "instance_name": "your-instance-name",
    "idle_timeout_seconds": 300,
    "dest_port": 80
}
```

### Configuration Options

- `credentials_file`: Path to your GCP service account credentials JSON file
- `project_id`: Your GCP project ID
- `default_zone`: The GCP zone where your instance is located
- `listen_port`: Port on which the proxy will listen for incoming requests
- `instance_name`: Name of the GCE instance to manage
- `idle_timeout_seconds`: Number of seconds of inactivity before stopping the instance
- `dest_port`: Port on the instance to forward requests to

## Logging

The proxy provides detailed logging of instance lifecycle events:

- Instance start attempts and current state
- Instance status changes (including IP address)
- Instance idle timeout detection and shutdown
- Instance stop operations (both manual and automatic)
- Graceful shutdown operations
- Error conditions and failures

Example log output:
```
Starting instance my-instance (current state: TERMINATED)
Instance my-instance state changed: STARTING -> RUNNING (IP: 1.2.3.4)
Instance my-instance idle for 5m0s, shutting down
Successfully stopped instance my-instance
```

## Building

```bash
go build -o proxyd cmd/proxyd/main.go
```

## Running

```bash
./proxyd
```

## API

### Health Check

```
GET /health
```

Returns a JSON response with the current instance state:

```json
{
    "status": "RUNNING",
    "ip": "1.2.3.4",
    "last_used": "2024-03-14T12:00:00Z",
    "start_time": "2024-03-14T11:55:00Z"
}
```

Possible status values:
- `TERMINATED`: Instance is stopped
- `STARTING`: Instance is in the process of starting
- `RUNNING`: Instance is running and ready to handle requests

## Instance Management

The proxy automatically manages the instance lifecycle:

1. When a request is received and the instance is stopped:
   - Instance is started
   - Request is queued with a 503 Service Unavailable response
   - Client should retry after the instance is running

2. While the instance is running:
   - All requests are forwarded to the instance
   - Last used timestamp is updated
   - Idle timeout is monitored

3. When the instance is idle:
   - After the configured timeout, instance is automatically stopped
   - Next request will trigger a new start cycle

4. During proxy shutdown:
   - Instance is gracefully stopped if running
   - All pending operations are completed

## Error Handling

- Failed instance operations are logged with detailed error messages
- Clients receive appropriate HTTP status codes:
  - 503 Service Unavailable when instance is starting
  - 500 Internal Server Error for unexpected failures
  - Original response from the instance for successful requests

## Dependencies

- Go 1.16 or later
- Google Cloud SDK
- GCP service account with appropriate permissions 