// vertex.go — minimal Vertex AI Gemini client. Auth path: service-account JSON →
// short-lived OAuth token (cloud-platform scope) → generateContent URL.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"golang.org/x/oauth2/google"
)

const vertexScope = "https://www.googleapis.com/auth/cloud-platform"

// VertexClient holds a cached OAuth token source plus the project/location needed to
// build generateContent URLs. Safe for concurrent use.
type VertexClient struct {
	projectID string
	location  string
	credJSON  []byte

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// NewVertexClient builds a client from service-account JSON contents. location
// defaults to "global".
func NewVertexClient(credJSON []byte, location string) (*VertexClient, error) {
	var meta struct {
		Type      string `json:"type"`
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(credJSON, &meta); err != nil {
		return nil, fmt.Errorf("parse service account json: %w", err)
	}
	if meta.Type != "service_account" || meta.ProjectID == "" {
		return nil, fmt.Errorf("not a service-account json (type=%q project_id=%q)", meta.Type, meta.ProjectID)
	}
	if _, err := google.JWTConfigFromJSON(credJSON, vertexScope); err != nil {
		return nil, fmt.Errorf("invalid service-account key: %w", err)
	}
	if location == "" {
		location = "global"
	}
	return &VertexClient{projectID: meta.ProjectID, location: location, credJSON: credJSON}, nil
}

// Token returns a cached, auto-refreshed OAuth access token for the service account.
func (c *VertexClient) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expiry.Add(-30*time.Second)) {
		return c.token, nil
	}
	conf, err := google.JWTConfigFromJSON(c.credJSON, vertexScope)
	if err != nil {
		return "", fmt.Errorf("service account: %w", err)
	}
	tok, err := conf.TokenSource(ctx).Token()
	if err != nil {
		return "", fmt.Errorf("mint token: %w", err)
	}
	c.token, c.expiry = tok.AccessToken, tok.Expiry
	return c.token, nil
}

// GenerateContentURL builds the generateContent endpoint for a model.
func (c *VertexClient) GenerateContentURL(model string) string {
	host := "aiplatform.googleapis.com"
	if c.location != "global" {
		host = c.location + "-aiplatform.googleapis.com"
	}
	return fmt.Sprintf("https://%s/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent",
		host, c.projectID, c.location, model)
}
