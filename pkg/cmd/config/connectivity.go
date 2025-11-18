// Copyright KubeArchive Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"io"
	"net/http"

	"k8s.io/client-go/rest"
)

// TestKubeArchiveConnectivity tests KubeArchive connectivity with the provided configuration
// Tests /api/v1/pods?limit=1 endpoint and considers 200, 404, or 400 as success
func TestKubeArchiveConnectivity(host string, tlsInsecure bool, token string, caData []byte) error {
	// Create REST config with the provided parameters
	restConfig := &rest.Config{
		Host: host,
		TLSClientConfig: rest.TLSClientConfig{
			Insecure: tlsInsecure,
		},
	}

	if token != "" {
		restConfig.BearerToken = token
	}

	if caData != nil {
		restConfig.TLSClientConfig.CAData = caData
	}

	// Use rest.HTTPClientFor to create client with proper configuration
	client, err := rest.HTTPClientFor(restConfig)
	if err != nil {
		return fmt.Errorf("failed to create HTTP client: %w", err)
	}

	// Test the /api/v1/pods?limit=1 endpoint
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/api/v1/pods?limit=1", host), nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}
	defer resp.Body.Close()

	// Consider 200 (OK), 404 (Not Found), and 401 (Unauthorized) as success
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNotFound, http.StatusUnauthorized:
		return nil // Success - server is reachable and responding
	default:
		// Read response body for better error messages
		body, _ := io.ReadAll(resp.Body)
		if len(body) > 0 {
			return fmt.Errorf("server returned status %d: %s", resp.StatusCode, string(body))
		}
		return fmt.Errorf("server returned status %d", resp.StatusCode)
	}
}

func TestKubeArchiveLivezEndpoint(host string, tlsInsecure bool, caData []byte) error {
	restConfig := &rest.Config{
		Host: host,
		TLSClientConfig: rest.TLSClientConfig{
			Insecure: tlsInsecure,
		},
	}
	if caData != nil {
		restConfig.CAData = caData
	}
	client, err := rest.HTTPClientFor(restConfig)
	if err != nil {
		return fmt.Errorf("failed to create HTTP client: %w", err)
	}

	// Test the /livez endpoint
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/livez", host), nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	return fmt.Errorf("server returned status %d", resp.StatusCode)
}
