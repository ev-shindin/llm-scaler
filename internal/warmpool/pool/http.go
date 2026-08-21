package pool

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
)

// doJSON is the one place a request is made, so timeouts, body limits and the
// preservation of the server's own error text are decided once.
func doJSON(ctx context.Context, client *http.Client, method, url string, body []byte) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, fmt.Errorf("build %s %s: %w", method, url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read %s %s: %w", method, url, err)
	}
	if resp.StatusCode >= 300 {
		return nil, statusError{code: resp.StatusCode, method: method, path: url, body: string(payload)}
	}
	return payload, nil
}
