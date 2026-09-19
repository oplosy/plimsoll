package bybit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// envelope is the shape every V5 response arrives in.
//
// Bybit reports business failure inside a 200 with a non-zero retCode, so a client that only
// checked the HTTP status would read "your key expired" as a successful empty page -- and a
// backfill would then record a complete history of nothing, with a cursor advanced past it.
type envelope struct {
	RetCode int             `json:"retCode"`
	RetMsg  string          `json:"retMsg"`
	Result  json.RawMessage `json:"result"`
}

// request is one signed GET.
type request struct {
	path  string
	query url.Values
	// cost is what it spends in the limiter.
	cost int
}

// do sends a signed GET and returns the `result` object, retrying only 5xx.
func (c *Client) do(ctx context.Context, req request) (json.RawMessage, error) {
	var lastErr error
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(c.backoff(attempt - 1)):
			}
		}
		result, err := c.attempt(ctx, req)
		if err == nil {
			return result, nil
		}
		// Only a transport or 5xx failure is worth sending again. A refused key, a rate
		// limit and a malformed request are all answered the same way by a retry: the same
		// refusal, one budget unit later.
		if !errors.Is(err, errRetryable) {
			return nil, err
		}
		lastErr = err
	}
	return nil, fmt.Errorf("bybit: %s failed after %d attempts: %w", req.path, MaxAttempts, lastErr)
}

var errRetryable = errors.New("bybit: retryable")

func (c *Client) attempt(ctx context.Context, req request) (json.RawMessage, error) {
	// Budget is spent BEFORE the request leaves, never after: asking forgiveness from a rate
	// limiter is asking the whole IP to wait (K24).
	if err := c.limiter.Acquire(ctx, c.integrationID, req.cost, c.priority); err != nil {
		return nil, fmt.Errorf("bybit: acquire budget for %s: %w", req.path, err)
	}

	query := req.query
	if query == nil {
		query = url.Values{}
	}
	timestamp, recv, signature, encoded := sign(
		c.cred.APISecret.Reveal(), c.cred.APIKey.Reveal(), c.now(), c.recvWindow, query)

	target := c.baseURL + req.path
	if encoded != "" {
		target += "?" + encoded
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("bybit: build request for %s: %w", req.path, err)
	}
	httpReq.Header.Set(headerAPIKey, c.cred.APIKey.Reveal())
	httpReq.Header.Set(headerTimestamp, timestamp)
	httpReq.Header.Set(headerRecvWindow, recv)
	httpReq.Header.Set(headerSign, signature)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// The URL is in the error and the credential is not: the path is a debugging aid,
		// the key is never one (L13).
		return nil, fmt.Errorf("%w: %s: %w", errRetryable, req.path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %w", errRetryable, req.path, err)
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("%w: %s, waiting %s", ErrRateLimited, req.path, unstatedPenalty)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%w: %s answered %d", ErrPermission, req.path, resp.StatusCode)
	case resp.StatusCode >= 500:
		return nil, fmt.Errorf("%w: %s answered %d", errRetryable, req.path, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("bybit: %s answered %d", req.path, resp.StatusCode)
	}

	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("bybit: decode %s envelope: %w", req.path, err)
	}
	if env.RetCode != 0 {
		// retMsg is the venue's own words and carries no credential; retCode is what a
		// caller can branch on.
		return nil, fmt.Errorf("%w: %s returned %d (%s)",
			ErrVenue, req.path, env.RetCode, env.RetMsg)
	}
	return env.Result, nil
}
