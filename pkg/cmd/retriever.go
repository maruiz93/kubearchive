// Copyright KubeArchive Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/pflag"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"

	"github.com/kubearchive/kubearchive/pkg/cmd/config"
)

// DiscoveryInterface defines the methods we need from the discovery client
type DiscoveryInterface interface {
	ServerGroups() (*v1.APIGroupList, error)
	ServerGroupsAndResources() ([]*v1.APIGroup, []*v1.APIResourceList, error)
}

// KARetrieverOptions implements KARetrieverCommand for get/logs commands and KACLICommand for the config command
type KARetrieverOptions struct {
	host            string
	tlsInsecure     bool
	certificatePath string
	token           string
	certData        []byte
	kubeFlags       *genericclioptions.ConfigFlags
	k8sRESTConfig   *rest.Config
	k9eRESTConfig   *rest.Config
	discoveryClient DiscoveryInterface
}

// NewKARetrieverOptionsNoEnv creates KARetrieverOptions without env vars
func NewKARetrieverOptionsNoEnv() *KARetrieverOptions {
	return &KARetrieverOptions{
		kubeFlags: genericclioptions.NewConfigFlags(true),
	}
}

// NewKARetrieverOptions creates KARetrieverOptions for retriever commands (get/logs) - loads env vars
func NewKARetrieverOptions() *KARetrieverOptions {
	opts := &KARetrieverOptions{
		host:            "", // No default host - must be configured
		certificatePath: "",
		kubeFlags:       genericclioptions.NewConfigFlags(true),
	}

	// Load environment variables for retriever commands
	if v := os.Getenv("KUBECTL_PLUGIN_KA_HOST"); v != "" {
		opts.host = v
	}
	if v := os.Getenv("KUBECTL_PLUGIN_KA_CERT_PATH"); v != "" {
		opts.certificatePath = v
	}
	if v := os.Getenv("KUBECTL_PLUGIN_KA_TLS_INSECURE"); v != "" {
		opts.tlsInsecure, _ = strconv.ParseBool(v)
	}
	if v := os.Getenv("KUBECTL_PLUGIN_KA_TOKEN"); v != "" {
		opts.token = v
	}

	return opts
}

// CompleteK8sConfig implements KACLICommand interface for Kubernetes configuration
func (opts *KARetrieverOptions) CompleteK8sConfig() error {
	// Load Kubernetes REST Config
	restConfig, err := opts.kubeFlags.ToRESTConfig()
	if err != nil {
		return fmt.Errorf("error creating the REST configuration: %w", err)
	}
	opts.k8sRESTConfig = restConfig
	return nil
}

// GetK8sRESTConfig returns the Kubernetes REST configuration
func (opts *KARetrieverOptions) GetK8sRESTConfig() *rest.Config {
	return opts.k8sRESTConfig
}

// CompleteRetriever implements the full retriever workflow for get/logs commands
func (opts *KARetrieverOptions) CompleteRetriever() error {
	// First complete the basic configuration
	if err := opts.CompleteK8sConfig(); err != nil {
		return err
	}

	client, err := opts.kubeFlags.ToDiscoveryClient()
	if err != nil {
		return fmt.Errorf("failed to create discovery client: %w", err)
	}
	opts.discoveryClient = client

	if opts.kubeFlags.BearerToken != nil && *opts.kubeFlags.BearerToken != "" {
		opts.token = *opts.kubeFlags.BearerToken
	}

	if opts.token == "" && opts.k8sRESTConfig.BearerToken != "" {
		opts.token = opts.k8sRESTConfig.BearerToken
	}

	if opts.certificatePath != "" {
		expandedCertPath, certData, err := config.LoadCertData(opts.certificatePath)
		if err != nil {
			return err
		}
		opts.certificatePath = expandedCertPath
		opts.certData = certData
	}

	// Check if there is a host for kubearchive
	if err = opts.testConnectivity(); err != nil {
		// If there isn't a host, load the config from persistent
		configManager := config.NewConfigManager()
		if err = opts.loadPersistentConfig(configManager); err != nil {
			return fmt.Errorf("failed to load persistent configuration: %w", err)
		}

		// If still no host after loading persistent config, offer interactive setup
		if opts.host == "" {
			fmt.Println("No KubeArchive configuration found.")
			fmt.Println()

			ns, err := opts.GetNamespace()
			if err != nil {
				ns = "default"
			}
			interactiveSetup := config.NewInteractiveSetup(configManager, ns)

			confirmation, err := config.PromptForConfirmation("Do you want to setup the configuration for the current connected cluster?", config.DefaultYes)
			if err != nil {
				return err
			}
			if !confirmation {
				return fmt.Errorf("setup configuration stopped by the user")
			}
			err = interactiveSetup.RunSetup()
			if err != nil {
				return fmt.Errorf("interactive setup failed: %w", err)
			}
			err = opts.loadPersistentConfig(configManager)
			if err != nil {
				return err
			}
		}
	}

	if err = opts.testConnectivity(); err != nil {
		return err
	}
	return opts.setK9eRESTConfig()
}

func (opts *KARetrieverOptions) testConnectivity() error {
	if opts.host == "" {
		return fmt.Errorf("no host provided")
	} else if opts.token == "" {
		return fmt.Errorf("no token provided")
	} else if err := config.TestKubeArchiveConnectivity(opts.host, opts.tlsInsecure, opts.token, opts.certData); err != nil {
		return err
	}
	return nil
}

// AddFlags adds all archive-related flags to the given flag set
func (opts *KARetrieverOptions) AddRetrieverFlags(flags *pflag.FlagSet) {
	opts.AddK8sFlags(flags)
	flags.StringVar(&opts.host, "host", opts.host, "host where the KubeArchive API Server is listening.")
	flags.BoolVar(&opts.tlsInsecure, "kubearchive-insecure-skip-tls-verify", opts.tlsInsecure, "Allow insecure requests to the KubeArchive API.")
	flags.StringVar(&opts.certificatePath, "kubearchive-certificate-authority", opts.certificatePath, "Path to the certificate authority file.")
}

func (opts *KARetrieverOptions) AddK8sFlags(flags *pflag.FlagSet) {
	opts.kubeFlags.AddFlags(flags)
}

// GetFromAPI retrieves data from either Kubernetes or KubeArchive API
func (opts *KARetrieverOptions) GetFromAPI(api API, path string) ([]byte, *APIError) {
	var restConfig *rest.Config
	var baseURL string

	switch api {
	case Kubernetes:
		restConfig = opts.k8sRESTConfig
		baseURL = restConfig.Host
	case KubeArchive:
		restConfig = opts.k9eRESTConfig
		baseURL = restConfig.Host
	default:
		return nil, &APIError{
			StatusCode: 500,
			URL:        path,
			Message:    "unknown API type",
			Body:       "",
		}
	}

	if restConfig == nil {
		return nil, &APIError{
			StatusCode: 500,
			URL:        path,
			Message:    "REST config not initialized",
			Body:       "",
		}
	}

	// Create HTTP client using the REST config
	client, err := rest.HTTPClientFor(restConfig)
	if err != nil {
		return nil, &APIError{
			StatusCode: 500,
			URL:        path,
			Message:    fmt.Sprintf("failed to create HTTP client: %v", err),
			Body:       "",
		}
	}

	// Build full URL
	fullURL := baseURL + path

	// Create request
	req, err := http.NewRequest("GET", fullURL, nil)
	if err != nil {
		return nil, &APIError{
			StatusCode: 500,
			URL:        fullURL,
			Message:    fmt.Sprintf("failed to create request: %v", err),
			Body:       "",
		}
	}

	// Add authorization header if we have a bearer token
	if restConfig.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+restConfig.BearerToken)
	}

	// Make the request
	resp, err := client.Do(req)
	if err != nil {
		return nil, &APIError{
			StatusCode: 0,
			URL:        fullURL,
			Message:    fmt.Sprintf("error on GET to %s: %v", fullURL, err),
			Body:       "",
		}
	}
	defer resp.Body.Close()

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			URL:        fullURL,
			Message:    fmt.Sprintf("failed to read response body: %v", err),
			Body:       "",
		}
	}

	// Check for HTTP errors
	if resp.StatusCode >= 400 {
		message := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if len(body) > 0 {
			message = fmt.Sprintf("%s (%d)", string(body), resp.StatusCode)
		}
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			URL:        fullURL,
			Message:    message,
			Body:       string(body),
		}
	}

	return body, nil
}

// GetNamespace get the provided namespace or the namespace used in kubeconfig context
func (opts *KARetrieverOptions) GetNamespace() (string, error) {
	if opts.kubeFlags.Namespace != nil && *opts.kubeFlags.Namespace != "" {
		return *opts.kubeFlags.Namespace, nil
	}
	if rawLoader := opts.kubeFlags.ToRawKubeConfigLoader(); rawLoader != nil {
		ns, _, nsErr := rawLoader.Namespace()
		if nsErr != nil {
			return "", fmt.Errorf("error retrieving namespace from kubeconfig context: %w", nsErr)
		}
		opts.kubeFlags.Namespace = &ns
		return ns, nil
	}
	return "", fmt.Errorf("unable to retrieve namespace from kubeconfig context")
}

// ResolveResourceSpec resolves a resource specification using Kubernetes discovery API
func (opts *KARetrieverOptions) ResolveResourceSpec(resourceSpec string) (*ResourceInfo, error) {
	if opts.discoveryClient == nil {
		return nil, fmt.Errorf("discovery client not initialized")
	}

	// Get all server resources
	_, apiResourceLists, err := opts.discoveryClient.ServerGroupsAndResources()
	if err != nil {
		return nil, fmt.Errorf("failed to get server resources: %w", err)
	}

	// Look for matching resource
	for _, apiResourceList := range apiResourceLists {
		for _, apiResource := range apiResourceList.APIResources {
			if opts.matchesResource(apiResource, resourceSpec) {
				return &ResourceInfo{
					Resource:     apiResource.Name,
					Version:      apiResourceList.GroupVersion,
					Group:        apiResource.Group,
					GroupVersion: apiResourceList.GroupVersion,
					Kind:         apiResource.Kind,
					Namespaced:   apiResource.Namespaced,
				}, nil
			}
		}
	}

	return nil, fmt.Errorf("resource %q not found", resourceSpec)
}

// matchesResource checks if an API resource matches the requested resource name
func (opts *KARetrieverOptions) matchesResource(apiResource v1.APIResource, resourceName string) bool {
	// Check exact match with resource name
	if apiResource.Name == resourceName {
		return true
	}

	// Check if it matches any of the short names
	for _, shortName := range apiResource.ShortNames {
		if shortName == resourceName {
			return true
		}
	}

	// Check singular name
	if apiResource.SingularName == resourceName {
		return true
	}

	// Check kind (case insensitive)
	if strings.EqualFold(apiResource.Kind, resourceName) {
		return true
	}

	return false
}

// loadPersistentConfig attempts to load configuration from the persistent config file
func (opts *KARetrieverOptions) loadPersistentConfig(configManager *config.ConfigManager) error {

	if err := configManager.LoadConfig(opts.k8sRESTConfig); err != nil {
		return nil
	}

	// Get cluster-specific configuration
	clusterConfig, err := configManager.GetCurrentClusterConfig()
	if err != nil {
		return nil // Ignore errors, use defaults
	}

	if clusterConfig != nil {
		opts.host = clusterConfig.Host
		opts.certificatePath = clusterConfig.CertPath
		opts.tlsInsecure = clusterConfig.TLSInsecure
		opts.token = clusterConfig.Token
		if opts.token == "" {
			opts.token = opts.k8sRESTConfig.BearerToken
		}
		if opts.certificatePath != "" {
			expandedCertPath, certData, err := config.LoadCertData(opts.certificatePath)
			if err != nil {
				return err
			}
			opts.certificatePath = expandedCertPath
			opts.certData = certData
		}
	}

	return nil
}

// setK9eRESTConfig completes the configuration setup with token resolution and k9eRESTConfig creation
func (opts *KARetrieverOptions) setK9eRESTConfig() error {

	opts.k9eRESTConfig = &rest.Config{
		Host: opts.host,
		TLSClientConfig: rest.TLSClientConfig{
			Insecure: opts.tlsInsecure,
		},
	}

	opts.k9eRESTConfig.BearerToken = opts.token
	if opts.certData != nil {
		opts.k9eRESTConfig.CAData = opts.certData
		opts.k9eRESTConfig.Insecure = false
	}

	return nil
}
