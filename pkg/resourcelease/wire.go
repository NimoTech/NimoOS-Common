package resourcelease

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// Internal error sentinels returned by do. Callers classify with errors.Is:
//
//	errNotSent   the request provably never reached the core (no URL file,
//	             dial failure); even a non-idempotent POST may be re-sent.
//	errTransient the core is unreachable or answered 5xx.
//	errDisabled  the core answered 90001 (scheduler disabled).
//	errRefused   any other 4xx (409, 429, ...).
//
// errNotSent wraps errTransient; errTransient, errDisabled and errRefused wrap
// ErrUnavailable. ErrNotFound (404 without the 90002 envelope), ErrLeaseGone,
// ErrBudgetRejected and ErrInvalidRequest do not.
var (
	errTransient = fmt.Errorf("%w: temporarily unreachable", ErrUnavailable)
	errNotSent   = fmt.Errorf("%w: request not sent", errTransient)
	errDisabled  = fmt.Errorf("%w: disabled", ErrUnavailable)
	errRefused   = fmt.Errorf("%w: refused", ErrUnavailable)
)

const (
	leasesPath           = "/v1/resources/leases"
	defaultHTTPTimeout   = 35 * time.Second // covers one 30 s long-poll
	errorBodyLimit       = 4096
	drainLimit           = 64 << 10
	envelopeSuccessValue = 200
)

// defaultHTTPClient is used when Options.HTTPClient is nil.
var defaultHTTPClient = &http.Client{Timeout: defaultHTTPTimeout}

// leaseWire is the lease object of a core response (envelope.data).
type leaseWire struct {
	ID               string `json:"id"`
	State            string `json:"state"`
	Device           string `json:"device"`
	ReleaseRequested bool   `json:"release_requested"`
}

type envelope struct {
	Success int             `json:"success"`
	Data    json.RawMessage `json:"data"`
}

// waitMsFor is the wait_ms of an acquire POST: 0 when the caller never waits,
// the long-poll when it waits without a bound, and the long-poll capped at
// MaxWait (at least 1 ms) when it waits a bounded time. pollWait <= 0 means
// MaxLongPoll; it is capped at MaxLongPoll.
func waitMsFor(r Request, pollWait time.Duration) int {
	if pollWait <= 0 || pollWait > MaxLongPoll {
		pollWait = MaxLongPoll
	}
	switch {
	case r.MaxWait == 0:
		return 0
	case r.MaxWait < 0:
		return int(pollWait / time.Millisecond)
	}
	w := min(pollWait, r.MaxWait)
	return max(int(w/time.Millisecond), 1)
}

// buildPayload is the acquire POST body. It is a map so encoding/json writes
// the keys sorted, byte for byte what the pre-SDK clients sent. Always sent:
// client, task, class, priority, tier ("" included), kind, device_pref,
// allow_fallback, est_vram_mb, est_ram_mb, wait_ms. The optional fields are
// only sent when set; budget is passed through untouched.
func buildPayload(client, defaultClass string, r Request, waitMs int) map[string]any {
	class := r.Class
	if class == "" {
		class = defaultClass
	}
	if class == "" {
		class = ClassLight
	}
	kind := r.Kind
	if kind == "" {
		kind = KindTask
	}
	devicePref, allowFallback := r.DevicePref, r.AllowFallback
	if len(devicePref) == 0 && len(r.Devices) > 0 {
		devicePref, allowFallback = r.Devices, true
	}
	p := map[string]any{
		"client":         client,
		"task":           r.Task,
		"class":          class,
		"priority":       r.Priority,
		"tier":           string(r.Tier),
		"kind":           kind,
		"device_pref":    devicePref,
		"allow_fallback": allowFallback,
		"est_vram_mb":    r.EstVRAMMB,
		"est_ram_mb":     r.EstRAMMB,
		"wait_ms":        waitMs,
	}
	if r.Workload != "" {
		p["workload"] = r.Workload
	}
	if r.SizeHint > 0 {
		p["size_hint"] = r.SizeHint
	}
	if r.FloorRAMMB > 0 {
		p["floor_ram_mb"] = r.FloorRAMMB
	}
	if r.FloorVRAMMB > 0 {
		p["floor_vram_mb"] = r.FloorVRAMMB
	}
	if r.PID > 0 {
		p["pid"] = r.PID
	}
	if r.TTLSec > 0 {
		p["ttl_s"] = r.TTLSec
	}
	if r.Budget != nil {
		p["budget"] = r.Budget
	}
	if r.AlreadyLoaded {
		p["already_loaded"] = true
	}
	if r.AlreadyRunning {
		p["already_running"] = true
	}
	return p
}

// heartbeatBody is the heartbeat body of HeartbeatWith: in_use and loaded
// always, last_used_at (RFC 3339 UTC) and tier only when set.
func heartbeatBody(info HeartbeatInfo) map[string]any {
	b := map[string]any{"in_use": info.InUse, "loaded": info.Loaded}
	if !info.LastUsedAt.IsZero() {
		b["last_used_at"] = info.LastUsedAt.UTC().Format(time.RFC3339)
	}
	if info.Tier != "" {
		b["tier"] = string(info.Tier)
	}
	return b
}

// releaseBody is the DELETE body of ReleaseWith, or nil (no body at all)
// when no peak was observed, whatever Shared says (as Parser: a 0 MB peak
// would drag the core's estimate down). Otherwise shared is always sent,
// and each peak only when > 0 (the Parser body sends just the peaks it
// measured).
func releaseBody(info ReleaseInfo) map[string]any {
	if info.ObservedPeakMB == 0 && info.ObservedPeakVRAMMB == 0 {
		return nil
	}
	b := map[string]any{"shared": info.Shared}
	if info.ObservedPeakMB > 0 {
		b["observed_peak_mb"] = info.ObservedPeakMB
	}
	if info.ObservedPeakVRAMMB > 0 {
		b["observed_peak_vram_mb"] = info.ObservedPeakVRAMMB
	}
	return b
}

// baseURL is Options.BaseURL, or the content of Options.URLFile re-read on
// every request (the core rewrites it on restart).
func (c *Client) baseURL() (string, error) {
	u := c.opts.BaseURL
	if u == "" {
		if c.opts.URLFile == "" {
			return "", errNotSent
		}
		b, err := os.ReadFile(c.opts.URLFile)
		if err != nil {
			return "", errNotSent
		}
		u = strings.TrimSpace(string(b))
		if u == "" {
			return "", errNotSent
		}
	}
	return strings.TrimRight(u, "/"), nil
}

func (c *Client) httpClient() *http.Client {
	if c.opts.HTTPClient != nil {
		return c.opts.HTTPClient
	}
	return defaultHTTPClient
}

// encodeBody returns the request body bytes; a nil body, including a typed
// nil map, is a zero-length body (never "null").
func encodeBody(body any) ([]byte, error) {
	switch b := body.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		if b == nil {
			return nil, nil
		}
	}
	return json.Marshal(body)
}

// send performs one request and classifies the answer. It returns the raw
// envelope data of a 2xx answer whose envelope says success 200, nil data
// for 204, and otherwise one of:
//
//	400 error_code=invalid_budget   ErrBudgetRejected
//	other 400                       ErrInvalidRequest
//	404 + 90002                     ErrLeaseGone
//	other 404                       ErrNotFound
//	90001 (any status)              errDisabled
//	5xx                             errTransient
//	other 4xx                       errRefused
//	2xx with a bad envelope         ErrUnavailable
//	no URL file / dial failure      errNotSent
//	other transport error           errTransient
//	transport error after ctx ended ErrUnavailable (also matching ctx.Err())
func (c *Client) send(ctx context.Context, method, path string, body any) (json.RawMessage, bool, error) {
	base, err := c.baseURL()
	if err != nil {
		return nil, false, err
	}
	payload, err := encodeBody(body)
	if err != nil {
		return nil, false, fmt.Errorf("%w: encode body: %v", ErrUnavailable, err)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(payload))
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, false, fmt.Errorf("%w: %w", ErrUnavailable, ctxErr)
		}
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op == "dial" {
			return nil, false, errNotSent
		}
		return nil, false, errTransient
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
		resp.Body.Close()
	}()

	switch {
	case resp.StatusCode == http.StatusBadRequest:
		var rejection struct {
			Code string `json:"error_code"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, errorBodyLimit)).Decode(&rejection)
		if rejection.Code == string(CodeInvalidBudget) {
			return nil, false, ErrBudgetRejected
		}
		return nil, false, ErrInvalidRequest
	case resp.StatusCode >= 400:
		var e envelope
		_ = json.NewDecoder(io.LimitReader(resp.Body, errorBodyLimit)).Decode(&e)
		switch {
		case resp.StatusCode == http.StatusNotFound && e.Success == CodeLeaseNotFound:
			return nil, false, ErrLeaseGone
		case resp.StatusCode == http.StatusNotFound:
			return nil, false, ErrNotFound
		case e.Success == CodeResourcesDisabled:
			return nil, false, errDisabled
		case resp.StatusCode >= 500:
			return nil, false, errTransient
		}
		return nil, false, errRefused
	case resp.StatusCode == http.StatusNoContent:
		return nil, true, nil
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, false, fmt.Errorf("%w: unexpected status %d", ErrUnavailable, resp.StatusCode)
	}
	var e envelope
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil || e.Success != envelopeSuccessValue {
		return nil, false, fmt.Errorf("%w: bad response envelope", ErrUnavailable)
	}
	return e.Data, false, nil
}

// do sends one lease request and decodes the lease of the answer: a
// leaseWire for 2xx (an empty one for 204), else the error send classified.
// A 2xx answer without a lease id is ErrUnavailable.
func (c *Client) do(ctx context.Context, method, path string, body any) (*leaseWire, error) {
	data, noContent, err := c.send(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	if noContent {
		return &leaseWire{}, nil
	}
	var lw leaseWire
	if err := json.Unmarshal(data, &lw); err != nil || lw.ID == "" {
		return nil, fmt.Errorf("%w: response without a lease", ErrUnavailable)
	}
	return &lw, nil
}
