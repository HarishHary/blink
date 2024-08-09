package helpers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/cenkalti/backoff/v4"
)

const (
	MaxRetryAttempts      = 5
	DefaultRequestTimeout = 3*time.Second + 50*time.Millisecond // Close to 3.05
	DefaultServiceURL     = "https://example.com/api"           // Replace with actual URL
)

type OutputRequestFailure struct {
	Response *http.Response
}

func (e *OutputRequestFailure) Error() string {
	return fmt.Sprintf("Output request failed with status code: %d", e.Response.StatusCode)
}

type RequestHelper struct{}

func (h *RequestHelper) RetryOnException(fn func() (*http.Response, error), exceptions []any) (*http.Response, error) {
	var resp *http.Response
	operation := func() error {
		resp, err := fn()
		if err != nil {
			for _, ex := range exceptions {
				if errors.As(err, &ex) {
					return err
				}
			}
			return backoff.Permanent(err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return &OutputRequestFailure{Response: resp}
		}
		return nil
	}

	// Define backoff strategy
	b := backoff.NewExponentialBackOff()
	b.MaxElapsedTime = MaxRetryAttempts * DefaultRequestTimeout

	err := backoff.Retry(operation, b)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (h *RequestHelper) PutRequest(url string, headers map[string]string, data any) (*http.Response, error) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: DefaultRequestTimeout}
	req, err := http.NewRequest("PUT", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return client.Do(req)
}

func (h *RequestHelper) GetRequest(url string, headers map[string]string) (*http.Response, error) {
	client := &http.Client{Timeout: DefaultRequestTimeout}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return client.Do(req)
}

func (h *RequestHelper) PostRequest(url string, headers map[string]string, data any) (*http.Response, error) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: DefaultRequestTimeout}
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return client.Do(req)
}

func (h *RequestHelper) PutRequestRetry(url string, headers map[string]string, data any, exceptions []any) (*http.Response, error) {
	return h.RetryOnException(func() (*http.Response, error) {
		resp, err := h.PutRequest(url, headers, data)
		if err != nil {
			return nil, err
		}
		success := h.CheckHTTPResponse(resp)
		if !success {
			return nil, &OutputRequestFailure{Response: resp}
		}
		return resp, nil
	}, exceptions)
}

func (h *RequestHelper) GetRequestRetry(url string, headers map[string]string, exceptions []any) (*http.Response, error) {
	return h.RetryOnException(func() (*http.Response, error) {
		resp, err := h.GetRequest(url, headers)
		if err != nil {
			return nil, err
		}
		success := h.CheckHTTPResponse(resp)
		if !success {
			return nil, &OutputRequestFailure{Response: resp}
		}
		return resp, nil
	}, exceptions)
}

func (h *RequestHelper) PostRequestRetry(url string, headers map[string]string, data any, exceptions []any) (*http.Response, error) {
	return h.RetryOnException(func() (*http.Response, error) {
		resp, err := h.PostRequest(url, headers, data)
		if err != nil {
			return nil, err
		}
		success := h.CheckHTTPResponse(resp)
		if !success {
			return nil, &OutputRequestFailure{Response: resp}
		}
		return resp, nil
	}, exceptions)
}

func (h *RequestHelper) CheckHTTPResponse(response *http.Response) bool {
	success := response != nil && (response.StatusCode >= 200 && response.StatusCode <= 299)
	if !success {
		fmt.Printf("Encountered an error while sending: %v", response)
	}
	return success
}

func (h *RequestHelper) CatchExceptions() []any {
	return []any{&OutputRequestFailure{}, &url.Error{}}
}
