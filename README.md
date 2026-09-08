# Rotating-ip
High-performance Go rotating proxy pool for web scraping with automatic health checks, latency-based proxy selection, cooldown, and periodic proxy refresh.

# Go Rotating Proxy

## Features
- Proxy pool
- Parallel health check
- Latency-based selection
- Automatic cooldown
- Automatic proxy refresh
- HTTP proxy
- HTTPS CONNECT
- Management API

## Requirements
- Ubuntu 22.04 / 24.04
- Go
- Internet connection

## Installation
1. Clone repository
2. Install Go
3. Build binary
4. Create configuration
5. Run service

## Configuration
PROXY_API_URL=...
LISTEN_ADDR=...
MANAGEMENT_ADDR=...
REFRESH_INTERVAL=...
HEALTH_TIMEOUT=...
API_TIMEOUT=...
CHECK_WORKERS=...
MAX_FAILURES=...
COOLDOWN_BASE=...

## Running
./rotator

## Testing
curl http://127.0.0.1:8090/health

## Using as Proxy
127.0.0.1:8080

## Remote Proxy
http://SERVER_IP:8080

## Management API
/health
/proxies
/refresh

## Systemd
/etc/systemd/system/rotator.service

## Firewall
ufw ...

## Troubleshooting
PROXY_API_URL belum di-set
ERR_CONNECTION_REFUSED
Too many open files
No healthy proxy
API timeout

## Security
- Never commit API token
- Use environment variables
- Restrict management API

## License
