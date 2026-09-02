package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxResponseBody       = 8 << 20
	defaultRequestTimeout = 30 * time.Second
)

type apiClient struct {
	baseURL    *url.URL
	apiKey     string
	httpClient *http.Client
}

type apiError struct {
	Method     string
	Path       string
	StatusCode int
	Message    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("Vultr API %s %s rejected request (%d): %s", e.Method, e.Path, e.StatusCode, e.Message)
}

func newAPIClient(baseURL, apiKey string, httpClient *http.Client) (*apiClient, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse Vultr API base URL: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("API base URL must be an absolute URL without a query or fragment")
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, errors.New("VULTR_API_KEY is required")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultRequestTimeout}
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return &apiClient{baseURL: parsed, apiKey: apiKey, httpClient: httpClient}, nil
}

func (c *apiClient) listLoadBalancers(ctx context.Context) ([]loadBalancer, error) {
	var all []loadBalancer
	cursor := ""
	seen := make(map[string]struct{})

	for {
		query := url.Values{"per_page": {"500"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}

		var response struct {
			LoadBalancers []loadBalancer `json:"load_balancers"`
			Meta          struct {
				Links struct {
					Next string `json:"next"`
				} `json:"links"`
			} `json:"meta"`
		}
		path := "/v2/load-balancers?" + query.Encode()
		if err := c.do(ctx, http.MethodGet, path, nil, &response); err != nil {
			return nil, err
		}
		all = append(all, response.LoadBalancers...)

		next := response.Meta.Links.Next
		if next == "" {
			return all, nil
		}
		if _, ok := seen[next]; ok {
			return nil, errors.New("API returned a repeated load-balancer pagination cursor")
		}
		seen[next] = struct{}{}
		cursor = next
	}
}

func (c *apiClient) getLoadBalancer(ctx context.Context, id string) (loadBalancer, error) {
	var response struct {
		LoadBalancer loadBalancer `json:"load_balancer"`
	}
	path := "/v2/load-balancers/" + url.PathEscape(id)
	if err := c.do(ctx, http.MethodGet, path, nil, &response); err != nil {
		return loadBalancer{}, err
	}
	return response.LoadBalancer, nil
}

func (c *apiClient) createLoadBalancer(ctx context.Context, request createLoadBalancerRequest) (loadBalancer, error) {
	var response struct {
		LoadBalancer loadBalancer `json:"load_balancer"`
	}
	if err := c.do(ctx, http.MethodPost, "/v2/load-balancers", request, &response); err != nil {
		return loadBalancer{}, err
	}
	return response.LoadBalancer, nil
}

func (c *apiClient) updateLoadBalancer(ctx context.Context, id string, request updateLoadBalancerRequest) error {
	path := "/v2/load-balancers/" + url.PathEscape(id)
	return c.do(ctx, http.MethodPatch, path, request, nil)
}

func (c *apiClient) do(ctx context.Context, method, path string, body, result any) error {
	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode Vultr API request: %w", err)
		}
		requestBody = bytes.NewReader(encoded)
	}

	target := *c.baseURL
	target.Path = strings.TrimRight(c.baseURL.Path, "/") + strings.SplitN(path, "?", 2)[0]
	if parts := strings.SplitN(path, "?", 2); len(parts) == 2 {
		target.RawQuery = parts[1]
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), requestBody)
	if err != nil {
		return fmt.Errorf("build Vultr API request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("User-Agent", "castlemilk-dns-vultr-lb/1")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call Vultr API: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return fmt.Errorf("read Vultr API response: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close Vultr API response: %w", closeErr)
	}
	if len(data) > maxResponseBody {
		return errors.New("API response exceeded 8 MiB")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return &apiError{
			Method:     method,
			Path:       strings.SplitN(path, "?", 2)[0],
			StatusCode: resp.StatusCode,
			Message:    apiErrorMessage(data, c.apiKey, resp.StatusCode),
		}
	}
	if result == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, result); err != nil {
		return fmt.Errorf("decode Vultr API response: %w", err)
	}
	return nil
}

func apiErrorMessage(data []byte, apiKey string, statusCode int) string {
	var response struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	message := ""
	if json.Unmarshal(data, &response) == nil {
		message = response.Error
		if message == "" {
			message = response.Message
		}
	}
	if message == "" {
		message = strings.TrimSpace(string(data))
	}
	if message == "" {
		message = http.StatusText(statusCode)
	}
	message = strings.Join(strings.Fields(message), " ")
	message = strings.ReplaceAll(message, "Bearer "+apiKey, "Bearer [REDACTED]")
	message = strings.ReplaceAll(message, apiKey, "[REDACTED]")
	if len(message) > 512 {
		message = message[:512] + "..."
	}
	return message
}

func pointerTo[T any](value T) *T {
	return &value
}

func integerString(value int) string {
	return strconv.Itoa(value)
}
