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
	PageSize       int    `json:"pageSize,omitempty"`
	ParallelJobs   int    `json:"parallelJobs,omitempty"`
	OutputID       string `json:"outputId,omitempty"`
	GroupName      string `json:"groupName,omitempty"`
	GroupMatch     string `json:"groupMatch,omitempty"`
	JsonOutputFile string `json:"jsonOutputFile,omitempty"` // The full path to the JSON output file
}

// LoadConfig loads the application's configuration from command-line flags
// and a JSON file, handling precedence correctly. It also handles the --version
// flag and exits if it is present.
func LoadConfig() (Config, error) {
	// --- Flag Definition ---
	configFilePath := flag.String("config", "", "Path to a JSON configuration file. Command-line flags override file values.")
	versionFlag := flag.Bool("version", false, "Print the version and exit.")
	pageSize := flag.Int("pageSize", 500, "The number of items to retrieve per page for API queries. Max is 999.")
	parallelJobs := flag.Int("parallelJobs", 16, "Number of concurrent jobs for processing groups.")
	outputID := flag.String("output-id", "", "Custom ID for output filenames (e.g., 'my-export').")
	groupName := flag.String("group-name", "", "Exact name of a single group (e.g., 'MyGroup') or a comma-separated list (e.g., 'Group1,Group2') to process.")
	groupMatch := flag.String("group-match", "", "Partial match for a group name. Use '*' as a wildcard. E.g., 'Proj*', '*Test*', 'Start*End'. Defaults to 'contains' if no wildcards. Quote argument to avoid shell globbing.")

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
		PageSize:     *pageSize,
		ParallelJobs: *parallelJobs,
		OutputID:     *outputID,
		GroupName:    *groupName,
		GroupMatch:   *groupMatch,
	}

	// Load from config file if provided. This overwrites the defaults.
	if *configFilePath != "" {
		file, err := os.ReadFile(*configFilePath)
		if err != nil {
			return Config{}, fmt.Errorf("error reading config file: %w", err)
		}
		// We unmarshal into the config struct, which already has default values.
		// Any field present in the JSON will overwrite the default.
		if err := json.Unmarshal(file, &config); err != nil {
			return Config{}, fmt.Errorf("error parsing config file: %w", err)
		}
	}

	// Re-apply any flags that were set on the command line to override the config file.
	isSet := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) {
		isSet[f.Name] = true
	})

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

	// --- Validation ---
	if config.PageSize > 999 || config.PageSize < 1 {
		return Config{}, fmt.Errorf("pageSize must be between 1 and 999")
	}
	if config.GroupName != "" && config.GroupMatch != "" {
		return Config{}, fmt.Errorf("--group-name and --group-match cannot be used at the same time")
	}

	return config, nil
}
