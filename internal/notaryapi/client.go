package notaryapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const productionEndpoint = "https://appstoreconnect.apple.com/notary/v2/submissions"
const maxResponse = 1 << 20
const maxLog = 16 << 20

// Only fixed codes cross the CLI boundary. Provider errors may contain credentials,
// private filenames, signed URLs, account inventory, or authentication headers.
type failure string

func (e failure) Error() string { return "notarization: " + string(e) }

type result struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type client struct {
	credentials    *credentials
	endpoint       string
	transport      http.RoundTripper
	now            func() time.Time
	pollInterval   time.Duration
	requestTimeout time.Duration
	upload         func(context.Context, uploadAttributes, *os.File, string) error
	persist        func(*os.File, string, []byte) error
}

func newClient(creds *credentials) *client {
	return &client{credentials: creds, endpoint: productionEndpoint,
		transport: http.DefaultTransport.(*http.Transport).Clone(), now: time.Now,
		pollInterval: 15 * time.Second, requestTimeout: 30 * time.Second, persist: atomicWrite}
}

func (c *client) httpClient(timeout time.Duration) *http.Client {
	return &http.Client{Transport: c.transport, Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return failure("redirect_refused") }}
}

func (c *client) request(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	token, err := c.credentials.token(c.now())
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return nil, failure("request_failed")
	}
	if method == http.MethodPost {
		// NewRequest recognizes bytes.Reader and supplies GetBody automatically.
		// Go's HTTP/2 transport can use it to replay even POST after a peer reset
		// or GOAWAY. A create outcome is ambiguous in those cases, so the body
		// must remain non-replayable. Safe GET retries retain their usual behavior.
		req.GetBody = nil
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient(c.requestTimeout).Do(req)
	if err != nil {
		return nil, failure("request_failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, failure("authentication_failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, failure("request_failed")
	}
	data, err := readBounded(resp.Body, maxResponse)
	if err != nil {
		return nil, failure("response_failed")
	}
	return data, nil
}

func (c *client) preflight(ctx context.Context) error {
	data, err := c.request(ctx, http.MethodGet, "", nil)
	if err != nil {
		return err
	}
	// Validate the history envelope without printing, retaining, or following it.
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(data, &envelope) != nil || len(envelope.Data) == 0 || envelope.Data[0] != '[' {
		return failure("invalid_response")
	}
	return nil
}

func (c *client) wait(ctx context.Context, id string, timeout time.Duration) (result, error) {
	r := result{ID: id, Status: "Unknown"}
	if !uuidPattern.MatchString(id) || timeout <= 0 || timeout > 24*time.Hour {
		return r, failure("invalid_arguments")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if ctx.Err() != nil {
			return r, contextFailure(ctx)
		}
		data, err := c.request(ctx, http.MethodGet, "/"+id, nil)
		if ctx.Err() != nil {
			return r, contextFailure(ctx)
		}
		if err != nil {
			return result{ID: id, Status: "Unknown"}, err
		}
		var response struct {
			Data struct {
				ID         string `json:"id"`
				Attributes struct {
					Status string `json:"status"`
				} `json:"attributes"`
			} `json:"data"`
		}
		if json.Unmarshal(data, &response) != nil || !equalID(response.Data.ID, id) {
			return result{ID: id, Status: "Unknown"}, failure("invalid_response")
		}
		switch response.Data.Attributes.Status {
		case "Accepted":
			r.Status = "Accepted"
			return r, nil
		case "Invalid", "Rejected":
			r.Status = response.Data.Attributes.Status
			return r, failure("submission_rejected")
		case "In Progress":
			r.Status = "In Progress"
		default:
			return result{ID: id, Status: "Unknown"}, failure("invalid_response")
		}
		timer := time.NewTimer(c.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return r, contextFailure(ctx)
		case <-timer.C:
		}
	}
}

func contextFailure(ctx context.Context) error {
	if ctx.Err() == context.Canceled {
		return failure("cancelled")
	}
	return failure("timeout")
}

func equalID(a, b string) bool {
	return uuidPattern.MatchString(a) && strings.EqualFold(a, b)
}

func (c *client) log(ctx context.Context, id, output string) error {
	if !uuidPattern.MatchString(id) || output == "" {
		return failure("invalid_arguments")
	}
	dir, err := openDirectory(filepath.Dir(output))
	if err != nil {
		return failure("invalid_output")
	}
	defer dir.Close()
	name := filepath.Base(output)
	// mktemp callers supply an existing empty private file. Validate its descriptor
	// before writing; never truncate an existing log, symlink, or hard-linked file.
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		return failure("invalid_output")
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0777 != 0600 || st.Size != 0 || st.Nlink != 1 || int(st.Uid) != os.Geteuid() {
		return failure("invalid_output")
	}
	data, err := c.request(ctx, http.MethodGet, "/"+id+"/logs", nil)
	if err != nil {
		return err
	}
	var response struct {
		Data struct {
			Attributes struct {
				URL string `json:"developerLogUrl"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &response) != nil {
		return failure("invalid_response")
	}
	u, err := url.Parse(response.Data.Attributes.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return failure("invalid_response")
	}
	ctx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	// A fresh request and client carry neither the Apple JWT nor a cookie jar.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return failure("log_failed")
	}
	resp, err := c.httpClient(c.requestTimeout).Do(req)
	if err != nil {
		return failure("log_failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return failure("log_failed")
	}
	data, err = readBounded(resp.Body, maxLog)
	if err != nil || !json.Valid(data) || len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		return failure("invalid_log")
	}
	if _, err = io.Copy(f, bytes.NewReader(data)); err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = dir.Sync()
	}
	if err != nil {
		return failure("log_persistence_failed")
	}
	return nil
}
