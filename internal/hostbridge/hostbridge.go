// Package hostbridge carries plugin→host JSON-RPC calls across the C ABI.
//
// The ABI layer in package main installs the raw caller; everything above this
// package talks in terms of the decoded envelope so it can be exercised in tests
// by installing a fake caller.
package hostbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// Method names understood by the host. They mirror sdk/pluginabi.
const (
	MethodHostHTTPDo  = "host.http.do"
	MethodHostLog     = "host.log"
	MethodHostAuthGet = "host.auth.get"
)

// RawCaller performs one plugin→host call: it marshals the request payload,
// crosses the ABI, and returns the raw response envelope bytes.
type RawCaller func(method string, payload []byte) ([]byte, error)

var (
	mu     sync.RWMutex
	caller RawCaller
)

// Install registers the ABI bridge. Called once from package main.
func Install(fn RawCaller) {
	mu.Lock()
	defer mu.Unlock()
	caller = fn
}

func installed() RawCaller {
	mu.RLock()
	defer mu.RUnlock()
	return caller
}

// Envelope is the success/error envelope every RPC exchanges.
type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *EnvelopeError  `json:"error,omitempty"`
}

// EnvelopeError describes a failed RPC.
type EnvelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// Error implements the error interface.
func (e *EnvelopeError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

// Call invokes a host method and decodes its result into out.
//
// out may be nil when the caller only cares that the call succeeded.
func Call(ctx context.Context, method string, request any, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fn := installed()
	if fn == nil {
		return fmt.Errorf("hostbridge: no ABI caller installed")
	}
	payload, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		return fmt.Errorf("hostbridge: marshal %s request: %w", method, errMarshal)
	}
	raw, errCall := fn(method, payload)
	if errCall != nil {
		return fmt.Errorf("hostbridge: %s: %w", method, errCall)
	}
	var envelope Envelope
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return fmt.Errorf("hostbridge: decode %s envelope: %w", method, errUnmarshal)
	}
	if !envelope.OK {
		if envelope.Error != nil {
			return envelope.Error
		}
		return fmt.Errorf("hostbridge: %s failed without an error body", method)
	}
	if out == nil || len(envelope.Result) == 0 {
		return nil
	}
	if errDecode := json.Unmarshal(envelope.Result, out); errDecode != nil {
		return fmt.Errorf("hostbridge: decode %s result: %w", method, errDecode)
	}
	return nil
}

// Log levels understood by the host. Anything unrecognized degrades to debug.
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

type hostLogRequest struct {
	Level   string         `json:"level,omitempty"`
	Message string         `json:"message,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// Log emits a line into the host's log. Failures are swallowed: a plugin must
// never fail a request because logging was unavailable.
func Log(level, message string, fields map[string]any) {
	if installed() == nil {
		return
	}
	_ = Call(context.Background(), MethodHostLog, hostLogRequest{
		Level:   level,
		Message: message,
		Fields:  fields,
	}, nil)
}

type hostHTTPRequest struct {
	Method  string      `json:"method,omitempty"`
	URL     string      `json:"url,omitempty"`
	Headers http.Header `json:"headers,omitempty"`
	Body    []byte      `json:"body,omitempty"`
}

type hostHTTPResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// HTTPDo performs one outbound request through the host transport policy.
//
// The host exposes no timeout knob here, so callers must impose their own
// deadline around this call; a request that outlives it keeps running until the
// underlying HTTP layer gives up.
func HTTPDo(ctx context.Context, method, url string, headers http.Header, body []byte) (int, http.Header, []byte, error) {
	req := hostHTTPRequest{Method: method, URL: url, Headers: headers, Body: body}
	var resp hostHTTPResponse
	if err := Call(ctx, MethodHostHTTPDo, req, &resp); err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Headers, resp.Body, nil
}
