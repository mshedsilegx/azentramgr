package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// Config holds the configuration options for the extractor.
type Config struct {
	AuthMethod     string `json:"auth,omitempty"`
	TenantID       string `json:"tenantId,omitempty"` // From config file/env, used for clientid auth
	ClientID       string `json:"clientId,omitempty"`
	ClientSecret   string `json:"clientSecret,omitempty"`
	PageSize       int    `json:"pageSize,omitempty"`
	ParallelJobs   int    `json:"parallelJobs,omitempty"`
	OutputID       string `json:"outputId,omitempty"`
	GroupName      string `json:"groupName,omitempty"`
	GroupMatch     string `json:"groupMatch,omitempty"`
	UseCache       string `json:"useCache,omitempty"`
	JsonOutputFile string `json:"jsonOutputFile,omitempty"` // The full path to the JSON output file

	// New flags
	HealthCheck   bool   `json:"healthCheck,omitempty"`
	GroupListOnly bool   `json:"groupListOnly,omitempty"`
	TenantIDFlag  string `json:"tenantIdFlag,omitempty"` // From -tenantid flag, overrides all
}

// LoadConfig loads the application's configuration from command-line flags
// and a JSON file, handling precedence correctly. It also handles the --version
// flag and exits if it is present.
func LoadConfig() (Config, error) {
	// --- Flag Definition ---
	auth := flag.String("auth", "azidentity", "Authentication method: 'azidentity' or 'clientid'.")
	configFilePath := flag.String("config", "", "Path to a JSON configuration file. Command-line flags override file values.")
	useCache := flag.String("use-cache", "", "Path to a SQLite DB file to use as a cache for queries instead of the Graph API.")
	versionFlag := flag.Bool("version", false, "Print the version and exit.")
	pageSize := flag.Int("pageSize", 500, "The number of items to retrieve per page for API queries. Max is 999.")
	parallelJobs := flag.Int("parallelJobs", 16, "Number of concurrent jobs for processing groups.")
	outputID := flag.String("output-id", "", "Custom ID for output filenames (e.g., 'my-export').")
	groupName := flag.String("group-name", "", "Exact name of a single group (e.g., 'MyGroup') or a comma-separated list (e.g., 'Group1,Group2') to process.")
	groupMatch := flag.String("group-match", "", "Partial match for a group name. Use '*' as a wildcard. E.g., 'Proj*', '*Test*', 'Start*End'. Defaults to 'contains' if no wildcards. Quote argument to avoid shell globbing.")
	// New flags
	healthCheck := flag.Bool("healthcheck", false, "Check connectivity and authentication, then exit.")
	groupListOnly := flag.Bool("group-list-only", false, "List groups only; do not fetch members.")
	tenantID := flag.String("tenantid", "", "Optional: Force a specific tenant ID, overriding auto-detection or config file.")

	// Custom usage message
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Azure Entra Extractor v%s\n", version)
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}

	flag.Parse()

	if *versionFlag {
		fmt.Printf("AZ Entra Extractor v%s\n", version)
		os.Exit(0)
	}

	// --- Configuration Loading & Merging ---
	// Start with default values from the flags themselves.
	config := Config{
		AuthMethod:    *auth,
		PageSize:      *pageSize,
		ParallelJobs:  *parallelJobs,
		OutputID:      *outputID,
		GroupName:     *groupName,
		GroupMatch:    *groupMatch,
		UseCache:      *useCache,
		HealthCheck:   *healthCheck,
		GroupListOnly: *groupListOnly,
		TenantIDFlag:  *tenantID,
	}

	// Load from config file if provided. This overwrites the defaults.
	if *configFilePath != "" {
		file, err := os.ReadFile(*configFilePath)
		if err != nil {
			return Config{}, fmt.Errorf("error reading config file: %w", err)
		}
		if err := json.Unmarshal(file, &config); err != nil {
			return Config{}, fmt.Errorf("error parsing config file: %w", err)
		}
	}

	// Load from environment variables. These overwrite file values but not flags.
	if val, ok := os.LookupEnv("TENANT_ID"); ok {
		config.TenantID = val
	}
	if val, ok := os.LookupEnv("CLIENT_ID"); ok {
		config.ClientID = val
	}
	if val, ok := os.LookupEnv("CLIENT_SECRET"); ok {
		config.ClientSecret = val
	}

	// Re-apply any flags that were set on the command line to override the config file/env vars.
	isSet := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) {
		isSet[f.Name] = true
	})

	if isSet["auth"] {
		config.AuthMethod = *auth
	}
	if isSet["pageSize"] {
		config.PageSize = *pageSize
	}
	if isSet["parallelJobs"] {
		config.ParallelJobs = *parallelJobs
	}
	if isSet["output-id"] {
		config.OutputID = *outputID
	}
	if isSet["group-name"] {
		config.GroupName = *groupName
	}
	if isSet["group-match"] {
		config.GroupMatch = *groupMatch
	}
	if isSet["use-cache"] {
		config.UseCache = *useCache
	}
	if isSet["healthcheck"] {
		config.HealthCheck = *healthCheck
	}
	if isSet["group-list-only"] {
		config.GroupListOnly = *groupListOnly
	}
	if isSet["tenantid"] {
		config.TenantIDFlag = *tenantID
	}

	// --- Validation ---
	if config.HealthCheck {
		// If healthcheck is true, no other flags should be processed for validation.
		return config, nil
	}

	if config.UseCache != "" {
		if _, err := os.Stat(config.UseCache); os.IsNotExist(err) {
			return Config{}, fmt.Errorf("cache file does not exist: %s", config.UseCache)
		}
		// Check for incompatible flags when using cache
		if isSet["auth"] {
			return Config{}, fmt.Errorf("--auth is incompatible with --use-cache")
		}
		if isSet["pageSize"] {
			return Config{}, fmt.Errorf("--pageSize is incompatible with --use-cache")
		}
		if isSet["parallelJobs"] {
			return Config{}, fmt.Errorf("--parallelJobs is incompatible with --use-cache")
		}
	}

	if config.AuthMethod != "azidentity" && config.AuthMethod != "clientid" {
		return Config{}, fmt.Errorf("invalid auth method: %s. Must be 'azidentity' or 'clientid'", config.AuthMethod)
	}

	if config.AuthMethod == "clientid" {
		if config.TenantID == "" {
			return Config{}, fmt.Errorf("TENANT_ID must be set via config file or environment variable for clientid auth")
		}
		if config.ClientID == "" {
			return Config{}, fmt.Errorf("CLIENT_ID must be set via config file or environment variable for clientid auth")
		}
		if config.ClientSecret == "" {
			return Config{}, fmt.Errorf("CLIENT_SECRET must be set via config file or environment variable for clientid auth")
		}
	}
	if config.PageSize > 999 || config.PageSize < 1 {
		return Config{}, fmt.Errorf("pageSize must be between 1 and 999")
	}
	if config.GroupName != "" && config.GroupMatch != "" {
		return Config{}, fmt.Errorf("--group-name and --group-match cannot be used at the same time")
	}

	return config, nil
}
