# Poor Man's Proxy

A lightweight HTTP proxy service that automatically manages GCP instances based on traffic. It starts instances when needed and shuts them down when idle, helping to reduce costs while maintaining service availability.

## Features

- **Automatic Instance Management**
  - Starts backend instances on first request
  - Shuts down instances after period of inactivity
  - Handles concurrent requests during instance startup

- **Smart Request Handling**
  - Returns 503 with retry header while instance is starting
  - Forwards requests to backend once instance is ready
  - Preserves original request headers and adds proxy headers

- **Configurable Timeouts**
  - Idle timeout for instance shutdown
  - Monitoring interval for instance state
  - Configurable shutdown delay

## Configuration

Create a `config.json` file with the following settings:

```json
{
    "credentials_file": "path/to/service-account.json",
    "project_id": "your-gcp-project",
    "default_zone": "us-central1-a",
    "listen_port": 8080,
    "instance_name": "your-backend-instance",
    "instance_port": 80,
    "idle_timeout_seconds": 300,
    "monitor_interval_secs": 30,
    "idle_shutdown_minutes": 15
}
```

### Configuration Options

- `credentials_file`: Path to GCP service account key file
- `project_id`: Your GCP project ID
- `default_zone`: GCP zone where instances are located
- `listen_port`: Port for the proxy to listen on
- `instance_name`: Name of the backend instance to manage
- `instance_port`: Port the backend instance listens on
- `idle_timeout_seconds`: Time before considering instance idle
- `monitor_interval_secs`: How often to check instance state
- `idle_shutdown_minutes`: Minutes to wait before shutting down idle instance

## Codebase Structure

```
.
├── cmd/
│   └── proxyd/
│       └── main.go         # Main entry point
├── proxy/
│   ├── proxy.go           # Core proxy implementation
│   ├── proxy_test.go      # Test suite
│   └── api/
│       └── gce.go         # GCP API client
└── config.json            # Configuration file
```

### Key Components

- **Server**: Main proxy server implementation
  - Handles HTTP requests
  - Manages instance lifecycle
  - Implements request forwarding

- **GCE API Client**: Google Cloud Engine interface
  - Starts/stops instances
  - Gets instance status and IP
  - Handles GCP authentication

- **Configuration**: Service settings
  - Loaded from JSON file
  - Validated at startup
  - Used throughout the service

## Building and Running

1. Build the service:
   ```bash
   go build -o proxyd ./cmd/proxyd
   ```

2. Run with configuration:
   ```bash
   ./proxyd -config config.json
   ```

## Testing

Run the test suite:
```bash
go test ./...
```

Tests cover:
- Instance lifecycle management
- Request handling
- Configuration loading
- Error handling
- Header forwarding
- Idle timeout behavior

## Deployment

1. Create a GCP service account with permissions:
   - `compute.instances.start`
   - `compute.instances.stop`
   - `compute.instances.get`

2. Deploy to a small VM in the same region as your backend

3. Configure the service with your settings

4. Run as a system service for reliability

## Monitoring

The service logs important events:
- Instance state changes
- Request handling
- Errors and warnings

Use standard logging tools to monitor the service.

## Contributing

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Run tests
5. Submit a pull request

## License

MIT License - See LICENSE file for details 