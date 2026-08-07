package ztnet

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	apiVersionPath = "/api/v1"
	authHeader     = "x-ztnet-auth"
	maxBody        = 1 << 20
)

// networkInfo is the subset of the ZTNET /api/v1/network/{id} response used
// by the plugin; it carries the IPv6 assignment mode flags read by the script
// via `.v6AssignMode`.
type networkInfo struct {
	ID           string `json:"id"`
	V6AssignMode struct {
		Sixplane bool `json:"6plane"`
		RFC4193  bool `json:"rfc4193"`
	} `json:"v6AssignMode"`
}

// member is one entry of the ZTNET /api/v1/network/{id}/member response.
type member struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Authorized    bool     `json:"authorized"`
	IPAssignments []string `json:"ipAssignments"`
}

// apiClient talks to a ZTNET server the same way zt2hosts.sh does.
type apiClient struct {
	base   string // e.g. "http://localhost:3000/api/v1"
	token  string
	client *http.Client
}

// newAPIClient builds a client for the given ZTNET address. The address may
// be a bare host (as API_ADDRESS in the script) or already include /api/v1.
func newAPIClient(addr, token string, insecureSkipVerify bool) *apiClient {
	base := strings.TrimRight(addr, "/")
	if !strings.HasSuffix(base, apiVersionPath) {
		base += apiVersionPath
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if insecureSkipVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 - opt-in for self-signed ZTNET certificates
	}
	return &apiClient{
		base:   base,
		token:  token,
		client: &http.Client{Transport: tr, Timeout: 20 * time.Second},
	}
}

func (c *apiClient) network(ctx context.Context, id string) (networkInfo, error) {
	var info networkInfo
	err := c.get(ctx, "/network/"+id, &info)
	return info, err
}

func (c *apiClient) members(ctx context.Context, id string) ([]member, error) {
	var raw json.RawMessage
	if err := c.get(ctx, "/network/"+id+"/member", &raw); err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty member response")
	}
	if trimmed[0] == '[' {
		var list []member
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return nil, err
		}
		return list, nil
	}
	// Some controller API versions return an object keyed by member ID.
	var byID map[string]member
	if err := json.Unmarshal(trimmed, &byID); err != nil {
		return nil, err
	}
	out := make([]member, 0, len(byID))
	for _, m := range byID {
		out = append(out, m)
	}
	return out, nil
}

func (c *apiClient) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set(authHeader, c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}

	// ZTNET reports failures with an {"error": "..."} body (the script greps
	// the body for "error"), usually together with a non-2xx status. Only the
	// error path is decoded, so successful responses are parsed exactly once.
	if resp.StatusCode != http.StatusOK {
		var errPayload struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &errPayload) == nil && errPayload.Error != "" {
			return fmt.Errorf("%s: %s", path, errPayload.Error)
		}
		return fmt.Errorf("%s: unexpected HTTP status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: decoding response: %w", path, err)
	}
	return nil
}
