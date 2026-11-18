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
	"strings"
	"time"
)

// connectClient calls Prusa Connect's public HTTP API as the person whose API
// credential the slicer sent. The module holds no permission of its own:
// Connect decides what that person may store and print, exactly as it does for
// the web app, and its audit names that person.
type connectClient struct {
	base *url.URL
	http *http.Client
}

func newConnectClient(base string) (*connectClient, error) {
	parsed, err := url.Parse(strings.TrimRight(base, "/") + "/")
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("connect_url %q is not an http(s) address", base)
	}

	// No overall timeout: a print answers when the file has reached the
	// printer, which takes minutes for a large file. The slicer's request
	// context cancels it.
	return &connectClient{base: parsed, http: &http.Client{Transport: &http.Transport{
		ResponseHeaderTimeout: 30 * time.Minute,
		IdleConnTimeout:       90 * time.Second,
	}}}, nil
}

// device is the part of Connect's Device this module reads.
type device struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *connectClient) devices(ctx context.Context, token string) ([]device, error) {
	var page struct {
		Items []device `json:"items"`
	}

	err := c.do(ctx, token, http.MethodGet, "api/v1/devices", url.Values{"limit": {"100"}}, nil, "", -1, &page)

	return page.Items, err
}

// stored is Connect's answer to an upload into a storage.
type stored struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// store uploads a file into a storage of the server.
func (c *connectClient) store(ctx context.Context, token, storage, path string, body io.Reader, size int64) (stored, error) {
	var answer stored

	err := c.do(ctx, token, http.MethodPut, "api/v1/storages/"+url.PathEscape(storage)+"/content",
		url.Values{"path": {path}}, body, "application/octet-stream", size, &answer)

	return answer, err
}

// printRequest is the body of POST /api/v1/devices/{device_id}/print.
type printRequest struct {
	Storage string `json:"storage"`
	Source  string `json:"source"`
	Path    string `json:"path"`
	Replace bool   `json:"replace"`
}

type printAnswer struct {
	Path    string `json:"path"`
	Command struct {
		OK            bool   `json:"ok"`
		FaultCode     string `json:"fault_code"`
		MachineReason string `json:"machine_reason"`
	} `json:"command"`
}

// print has Connect send a stored file to the printer and start it. It answers
// when the file has arrived and the printer took (or refused) the start.
func (c *connectClient) print(ctx context.Context, token, deviceID string, request printRequest) (printAnswer, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return printAnswer{}, err
	}

	var answer printAnswer

	err = c.do(ctx, token, http.MethodPost, "api/v1/devices/"+url.PathEscape(deviceID)+"/print", nil,
		bytes.NewReader(body), "application/json", int64(len(body)), &answer)
	if err != nil {
		return answer, err
	}

	if !answer.Command.OK {
		reason := answer.Command.MachineReason
		if reason == "" {
			reason = answer.Command.FaultCode
		}

		return answer, &httpError{http.StatusConflict, "the file is on the printer, but the printer refused to start it: " + reason}
	}

	return answer, nil
}

// connectError is a refusal Connect answered, in its problem-details shape.
type connectError struct {
	Status int
	Code   string `json:"code"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

func (e *connectError) Error() string {
	message := e.Title
	if e.Detail != "" && e.Detail != e.Title {
		message += ": " + e.Detail
	}

	if e.Code != "" {
		message += " (" + e.Code + ")"
	}

	return message
}

func (c *connectClient) do(ctx context.Context, token, method, path string, query url.Values, body io.Reader,
	contentType string, size int64, into any,
) error {
	target := c.base.JoinPath(path)
	if query != nil {
		target.RawQuery = query.Encode()
	}

	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return err
	}

	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", name+"/"+string(release))

	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}

	if size >= 0 {
		request.ContentLength = size
	}

	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("cannot reach Prusa Connect at %s: %w", c.base, err)
	}
	defer response.Body.Close()

	if response.StatusCode >= 300 {
		refusal := &connectError{Status: response.StatusCode}

		raw, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		if json.Unmarshal(raw, refusal) != nil || refusal.Title == "" {
			refusal.Title = strings.TrimSpace(string(raw))
			if refusal.Title == "" {
				refusal.Title = response.Status
			}
		}

		return refusal
	}

	if into == nil {
		return nil
	}

	if err = json.NewDecoder(response.Body).Decode(into); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read Prusa Connect's answer to %s %s: %w", method, path, err)
	}

	return nil
}
