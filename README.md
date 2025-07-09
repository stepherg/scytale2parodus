# Scytale2Parodus

Scytale2Parodus is a Go-based HTTP server that acts as a bridge between requests and a Scytale service using the Web Request/Response Protocol (WRP). It exposes a POST endpoint to forward requests to a Scytale service, wrapping them in WRP messages, and returns the service's response.

## Table of Contents

- [Scytale2Parodus](#scytale2parodus)
  - [Table of Contents](#table-of-contents)
  - [Features](#features)
  - [Getting Started](#getting-started)
    - [Prerequisites](#prerequisites)
    - [Building](#building)
    - [Running](#running)
    - [Usage](#usage)
      - [Sending a Request](#sending-a-request)
  - [Configuration](#configuration)
  - [Endpoints](#endpoints)
  - [Metrics](#metrics)
  - [License](#license)

## Features

  * **HTTP to WRP Translation:** Converts JSON HTTP requests into WRP Msgpack messages.
  * **Scytale Integration:** Forwards WRP messages to a configurable Scytale endpoint.
  * **Request Body Size Limiting:** Prevents excessively large request bodies.
  * **Rate Limiting:** Protects the service from being overwhelmed by too many requests.
  * **Prometheus Metrics:** Exposes HTTP request duration and total request count.
  * **Health Check:** Provides an endpoint to check the reachability of the Scytale service.
  * **Configurable:** Supports configuration via environment variables, config files, and command-line flags.

## Getting Started

### Prerequisites

  * Go (version 1.20 or higher recommended)

### Building

To build the executable, navigate to the project root and run:

```bash
go build -o scytale2parodus .
```

This will create an executable named `scytale2parodus` in the current directory.

### Running

You can run the application directly:

```bash
./scytale2parodus
```

Or with specific flags:

```bash
./scytale2parodus --address ":8080" --scytale-url "http://localhost:6300/api/v2/device" --log-level "debug"
```

### Usage

#### Sending a Request
The server accepts POST requests to `/api/v1/{deviceID}/send/{service}`, where `deviceID` is a valid non-colon separated MAC address (e.g., `001122334455`) and `service` is the name of the service registered with parodus. The request body must be a JSON object with arbitrary key/value pairs. If the body contains the key `jsonrpc`, `id` will be set to the transaction uuid of the WRP message unless explicitly set in the body.

Example:
```bash
curl -X POST http://localhost:4900/api/v1/001122334455/send/Tasker \
     -H "Content-Type: application/json" \
     -d '{"jsonrpc":"2.0", "Tasker","method":"rbus.get","params":{"path":"Device.DeviceInfo.SerialNumber"}}'
```

Example to override the JSON-RPC id:
```bash
curl -X POST http://localhost:4900/api/v1/001122334455/send/Tasker \
     -H "Content-Type: application/json" \
     -d '{"jsonrpc":"2.0", "method":"rbus.get","params":{"path":"Device.DeviceInfo.SerialNumber"}, "id":"myid"}'
```

Response (example):
```json
{
  "jsonrpc": "2.0",
  "id": "7674ff29-e62c-45b8-a6c0-d70affdffc0f",
  "result": {
    "path": "Device.DeviceInfo.SerialNumber",
    "status": "success",
    "value": "CAA550A91EAA"
  }
}
```

## Configuration

The application can be configured using:

1.  **Environment Variables:** Prefixed with `SCYTALE2PARODUS_` (e.g., `SCYTALE2PARODUS_SCYTALE_URL`).
2.  **Configuration File:** `config.yaml` (or `config.json`, etc.) in the current directory or `/etc/scytale2parodus/`.
3.  **Command-line Flags:** As shown in the "Running" section.

Precedence: Command-line flags \> Environment Variables \> Config File \> Default values.

| Key | Environment Variable | Flag | Default Value | Description |
| :----------------- | :---------------------------- | :----------------------- | :------------------------------------- | :-------------------------------------------------- |
| `scytale_url` | `SCYTALE2PARODUS_SCYTALE_URL` | `--scytale-url` | `http://scytale:6300/api/v2/device` | URL of the Scytale device API endpoint. |
| `address` | `SCYTALE2PARODUS_ADDRESS` | `--address` | `:4900` | Address and port for the proxy to listen on. |
| `timeout` | `SCYTALE2PARODUS_TIMEOUT` | `--timeout` | `15` | Server read and write timeout in seconds. |
| `scytale_auth` | `SCYTALE2PARODUS_SCYTALE_AUTH`| `--scytale-auth` | `dXNlcjpwYXNz` (base64 encoded `user:pass`)| Basic authentication header for Scytale requests. |
| `log_level` | `SCYTALE2PARODUS_LOG_LEVEL` | `--log-level` | `info` | Log level (`debug`, `info`, `warn`, `error`). |
| `rate_limit` | `SCYTALE2PARODUS_RATE_LIMIT` | `--rate-limit` | `100.0` | Requests per second for rate limiting. |
| `rate_limit_burst` | `SCYTALE2PARODUS_RATE_LIMIT_BURST`| `--rate-limit-burst` | `200` | Burst size for rate limiting. |
| `shutdown_timeout` | `SCYTALE2PARODUS_SHUTDOWN_TIMEOUT`| (N/A) | `5` | Timeout for graceful server shutdown in seconds. |

**Example `config.yaml`:**

```yaml
scytale_url: "http://localhost:6300/api/v2/device"
address: ":8080"
timeout: 30
scytale_auth: "dXNlcjpwYXNz"
log_level: "debug"
rate_limit: 50
rate_limit_burst: 100
shutdown_timeout: 10
```

## Endpoints
  * **`POST /api/v1/{deviceID}/send/{service}`**
      * **Description:** Forwards an RPC request to the specified device and service via Scytale.
      * **Path Parameters:**
          * `deviceID`: A 12-character hexadecimal MAC address (e.g., `001122334455`).
          * `service`: The target service on the device (e.g., `tasker`).
      * **Request Body:** JSON payload representing the RPC request. If it's a JSON-RPC message and the `id` field is missing, a new UUID will be generated and added.
      * **Response:** JSON payload from the Scytale response.
      * **Status Codes:**
          * `200`: Success
          * `400`: Invalid device ID, service, or JSON
          * `429`: Rate limit exceeded
          * `502`: Empty Scytale response
          * `503`: Scytale unreachable
  * **`GET /health`**
      * **Description:** Checks if the Scytale service is reachable via a TCP connection.
      * **Response:** `{"status":"healthy"}` if reachable, otherwise an error.
      * **Status Codes:**
          * `200`: Healthy
          * `500`: Invalid Scytale URL
          * `503`: Scytale unreachable
  * **`GET /metrics`**
      * **Description:** Exposes Prometheus metrics for the application.

## Metrics

The application exposes the following Prometheus metrics at the `/metrics` endpoint:

  * `http_request_duration_seconds`: A histogram of HTTP request latencies, labeled by `handler`, `method`, `status`, and `path`.
  * `http_requests_total`: A counter for the total number of HTTP requests, labeled by `handler`, `method`, `status`, and `path`.

**Note:** Request duration and counter metrics are sampled at a 10% rate to reduce overhead.

## License

This project is licensed under the Apache License, Version 2.0. See the [LICENSE](LICENSE) file for details.
