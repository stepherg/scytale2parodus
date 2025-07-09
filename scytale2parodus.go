package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/xmidt-org/wrp-go/v3"
	"go.uber.org/zap"
)

const (
	MaxBodySize = 1 << 20 // 1MB
)

type RpcRequest map[string]any

type DeviceServer struct {
	log         *zap.Logger
	scytaleUrl  string
	scytaleAuth string
	client      *http.Client
}

// deviceIdRegex matches non-colon-separated MAC addresses (e.g., "001122334455")
var deviceIdRegex = regexp.MustCompile(`^[0-9A-Fa-f]{12}$`)

func NewDeviceServer(log *zap.Logger, scytaleUrl string, scytaleAuth string) (*DeviceServer, error) {
	return &DeviceServer{
		log:         log,
		scytaleUrl:  scytaleUrl,
		scytaleAuth: scytaleAuth,
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
				ForceAttemptHTTP2:   true,
			},
		},
	}, nil
}

func (u *DeviceServer) PostCallHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	deviceID := vars["deviceID"]
	service := vars["service"]

	// Validate deviceID
	if !deviceIdRegex.MatchString(deviceID) {
		u.log.Error("Invalid device ID format", zap.String("device_id", deviceID))
		http.Error(w, "Invalid device ID: must be a 12-character hexadecimal MAC address", http.StatusBadRequest)
		return
	}

	// Limit request body size
	bodyReader := io.LimitReader(r.Body, MaxBodySize)
	body, err := io.ReadAll(bodyReader)
	r.Body.Close()
	if err != nil {
		u.log.Error("Failed to read request body", zap.String("device_id", deviceID), zap.Error(err))
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	var deviceRequest RpcRequest
	if err = json.Unmarshal(body, &deviceRequest); err != nil {
		u.log.Error("Failed to unmarshal JSON", zap.String("device_id", deviceID), zap.Error(err))
		http.Error(w, "Failed to parse JSON", http.StatusBadRequest)
		return
	}

	requestID := uuid.New().String()
	// If this a json-rpc message, set the 'id' if not passed in the body
	if _, ok := deviceRequest["jsonrpc"].(string); ok {
		if id, ok := deviceRequest["id"].(string); !ok || id == "" {
			deviceRequest["id"] = requestID
		}
	}

	destination := fmt.Sprintf("mac:%s/%s", deviceID, service)
	logEntry := u.log.With(
		zap.String("device_id", deviceID),
		zap.String("request_id", requestID),
	)

	wrpPayload, err := json.Marshal(deviceRequest)
	if err != nil {
		logEntry.Error("Failed to marshal JSON", zap.Error(err))
		http.Error(w, "Failed to marshal JSON", http.StatusInternalServerError)
		return
	}

	wrpRequest := wrp.Message{
		Type:            wrp.SimpleRequestResponseMessageType,
		Source:          "scytale2parodus",
		Destination:     destination,
		ContentType:     "application/json",
		TransactionUUID: requestID,
		Payload:         wrpPayload,
	}

	wrpReqBody := bytes.NewBuffer(nil)
	if err := wrp.NewEncoder(wrpReqBody, wrp.Msgpack).Encode(wrpRequest); err != nil {
		logEntry.Error("Failed to encode WRP Msgpack", zap.Error(err))
		http.Error(w, "Failed to encode WRP Msgpack", http.StatusInternalServerError)
		return
	}
	postReq, err := http.NewRequest("POST", u.scytaleUrl, wrpReqBody)
	if err != nil {
		logEntry.Error("Failed to create HTTP request", zap.Error(err))
		http.Error(w, "Failed to post to Scytale", http.StatusServiceUnavailable)
		return
	}
	postReq.Header.Set("Content-Type", "application/msgpack")
	postReq.Header.Set("Accept", "application/msgpack")
	postReq.Header.Set("Authorization", "Basic "+u.scytaleAuth)

	logEntry.Debug("Sending WRP request", zap.String("url", u.scytaleUrl))
	resp, err := u.client.Do(postReq)
	if err != nil {
		logEntry.Error("Failed to post to Scytale", zap.Error(err))
		http.Error(w, "Failed to post to Scytale", http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		logEntry.Error("Scytale returned invalid status",
			zap.Int("status_code", resp.StatusCode),
			zap.String("status", resp.Status))
		http.Error(w, fmt.Sprintf("Scytale returned invalid status: %s", resp.Status), resp.StatusCode)
		return
	}

	var wrpResponse wrp.Message
	if err = wrp.NewDecoder(resp.Body, wrp.Msgpack).Decode(&wrpResponse); err != nil {
		logEntry.Error("Failed to decode Msgpack response", zap.Error(err))
		http.Error(w, "Failed to decode Scytale response", http.StatusBadRequest)
		return
	}
	logEntry.Debug("Received WRP response", zap.Any("response", wrpResponse))

	if len(wrpResponse.Payload) == 0 {
		logEntry.Warn("Empty payload in Scytale response")
		http.Error(w, "Empty response from Scytale", http.StatusBadGateway)
		return
	}

	// Set response headers and write payload
	w.Header().Set("Content-Type", "application/json")
	if _, err = w.Write(wrpResponse.Payload); err != nil {
		logEntry.Error("Failed to write response", zap.Error(err))
		// Response already started, can't set status
	}
}

// HealthCheckHandler checks if the Scytale server is reachable
func (u *DeviceServer) HealthCheckHandler(w http.ResponseWriter, r *http.Request) {
	// Parse Scytale URL to extract host and port
	parsedURL, err := url.Parse(u.scytaleUrl)
	if err != nil {
		u.log.Error("Failed to parse Scytale URL", zap.Error(err))
		http.Error(w, "Health check failed: invalid Scytale URL", http.StatusInternalServerError)
		return
	}

	host, port, err := net.SplitHostPort(parsedURL.Host)
	if err != nil {
		u.log.Error("Failed to extract host and port from Scytale URL", zap.Error(err))
		http.Error(w, "Health check failed: invalid Scytale host/port", http.StatusInternalServerError)
		return
	}

	// Attempt TCP connection to Scytale
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 2*time.Second)
	if err != nil {
		u.log.Error("Failed to connect to Scytale", zap.String("host", host), zap.String("port", port), zap.Error(err))
		http.Error(w, "Scytale unreachable", http.StatusServiceUnavailable)
		return
	}
	conn.Close()

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"healthy"}`))
}
