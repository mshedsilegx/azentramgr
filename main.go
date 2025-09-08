package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	_ "github.com/glebarez/sqlite"
	"github.com/golang-jwt/jwt/v5"
	abstractions "github.com/microsoft/kiota-abstractions-go"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	msgraphgocore "github.com/microsoftgraph/msgraph-sdk-go-core"
	"github.com/microsoftgraph/msgraph-sdk-go/groups"
	"github.com/microsoftgraph/msgraph-sdk-go/models"
)

var version = "dev"

// JSONMember represents the detailed structure for a user member in the JSON output.
type JSONMember struct {
	GivenName         string `json:"givenName,omitempty"`
	Mail              string `json:"mail,omitempty"`
	Surname           string `json:"surname,omitempty"`
	UserPrincipalName string `json:"userPrincipalName"`
}

// JSONGroup represents the structure for each group in the final JSON output.
type JSONGroup struct {
	ADGroupName       string       `json:"ADGroupName"`
	ADGroupMemberName []JSONMember `json:"ADGroupMemberName,omitempty"` // Use omitempty to hide if nil/empty
}

// SQLiteGroupMember represents a single row in the SQLite database.
type SQLiteGroupMember struct {
	GroupName  string
	MemberName string // This will now store the UserPrincipalName for users
}

// SQLiteUser represents a single row in the new entraUsers table.
type SQLiteUser struct {
	UserPrincipalName string
	GivenName         string
	Mail              string
	Surname           string
}

// Config holds the configuration options for the extractor.
type Config struct {
	PageSize       int    `json:"pageSize,omitempty"`
	ParallelJobs   int    `json:"parallelJobs,omitempty"`
	OutputID       string `json:"outputId,omitempty"`
	GroupName      string `json:"groupName,omitempty"`
	GroupMatch     string `json:"groupMatch,omitempty"`
	JsonOutputFile string `json:"jsonOutputFile,omitempty"` // The full path to the JSON output file
}

// Extractor holds the application's state and logic.
type Extractor struct {
	config         Config
	ctx            context.Context
	client         *msgraphsdk.GraphServiceClient
	db             *sql.DB
	limiter        *rate.Limiter
	tenantID       string
	totalGroups    atomic.Int64
	processedCount atomic.Int64
	countIsReady   atomic.Bool
}

// NewExtractor creates and initializes a new Extractor.
func NewExtractor(ctx context.Context, config Config, client *msgraphsdk.GraphServiceClient, db *sql.DB, tenantID string) (*Extractor, error) {
	return &Extractor{
		config:   config,
		ctx:      ctx,
		client:   client,
		db:       db,
		limiter:  rate.NewLimiter(rate.Limit(15), 1),
		tenantID: tenantID,
	}, nil
}

// Run executes the main logic of the extractor.
func (e *Extractor) Run() error {
	defer e.db.Close()

	// 1. Get total count of groups for progress reporting.
	totalGroupsInTenant, err := e.getGroupCount()
	if err != nil {
		return fmt.Errorf("could not get total group count: %w", err)
	}

	// Adjust logging based on filter type
	if e.config.GroupName != "" {
		log.Printf("Found %d group(s) matching the provided exact name(s). Starting extraction.", totalGroupsInTenant)
	} else if e.config.GroupMatch != "" {
		log.Printf("Found %d group(s) matching the partial search. Starting extraction.", totalGroupsInTenant)
	} else {
		log.Printf("Found %d total groups in tenant. Starting full scan...", totalGroupsInTenant)
	}

	// 2. Setup channels and wait groups for concurrent processing
	groupTasks := make(chan *models.Group, 100)
	jsonResults := make(chan JSONGroup, 100)
	sqliteResults := make(chan SQLiteGroupMember, 100)
	userTasks := make(chan SQLiteUser, 100)
	var workersWg, aggregatorsWg sync.WaitGroup

	// 3. Start workers and result aggregators
	numWorkers := e.config.ParallelJobs
	for i := 0; i < numWorkers; i++ {
		workersWg.Add(1)
		go e.worker(&workersWg, groupTasks, jsonResults, sqliteResults, userTasks)
	}
	aggregatorsWg.Add(1)
	go e.processSQLiteInserts(&aggregatorsWg, sqliteResults)
	aggregatorsWg.Add(1)
	go e.processUserInserts(&aggregatorsWg, userTasks)
	aggregatorsWg.Add(1)
	go e.streamJsonToFile(&aggregatorsWg, jsonResults)

	// 4. Goroutine to close result channels once all workers are done
	go func() {
		workersWg.Wait()
		close(jsonResults)
		close(sqliteResults)
		close(userTasks)
	}()

	// 5. Create a new WaitGroup for the dispatcher and run it in a goroutine
	var dispatcherWg sync.WaitGroup
	dispatcherWg.Add(1)
	go func() {
		defer dispatcherWg.Done()
		defer close(groupTasks) // Close channel when iteration is done

		pageIterator, err := e.getGroupIterator()
		if err != nil {
			log.Printf("Error getting group iterator: %v", err)
			return
		}

		scannedCount := 0
		matchingCount := 0
		err = pageIterator.Iterate(e.ctx, func(group *models.Group) bool {
			scannedCount++
			if scannedCount%500 == 0 {
				log.Printf("Scanning groups... [%d/%d]", scannedCount, totalGroupsInTenant)
			}

			// All filtering is now done server-side via the $filter query parameter.
			// Every group returned by the iterator is a match.
			matchingCount++
			groupTasks <- group
			return true
		})
		if err != nil {
			log.Printf("Error during group scan: %v", err)
		}
		e.totalGroups.Store(int64(matchingCount))
		e.countIsReady.Store(true)
		log.Printf("Scan complete. Found and dispatched %d matching groups for processing.", matchingCount)
	}()

	// 6. Wait for the dispatcher to finish sending all groups, then wait for aggregators
	dispatcherWg.Wait()
	log.Println("Finished dispatching all matched groups.")
	aggregatorsWg.Wait()
	log.Println("Finished aggregating all results.")

	return nil
}

func (e *Extractor) getGroupCount() (int32, error) {
	headers := abstractions.NewRequestHeaders()
	headers.Add("ConsistencyLevel", "eventual")

	// Create a new request configuration
	requestConfiguration := &groups.CountRequestBuilderGetRequestConfiguration{
		Headers: headers,
	}

	// Build the filter string based on the command-line flags
	var filter *string
	if e.config.GroupName != "" {
		groupNames := strings.Split(e.config.GroupName, ",")
		var filterClauses []string
		for _, name := range groupNames {
			trimmedName := strings.TrimSpace(name)
			if trimmedName != "" {
				sanitizedName := strings.ReplaceAll(trimmedName, "'", "''")
				filterClauses = append(filterClauses, fmt.Sprintf("displayName eq '%s'", sanitizedName))
			}
		}
		if len(filterClauses) > 0 {
			filter = strPtr(strings.Join(filterClauses, " or "))
		}
	} else if e.config.GroupMatch != "" {
		matchStr := strings.TrimSpace(e.config.GroupMatch)
		sanitizedMatchStr := strings.ReplaceAll(matchStr, "'", "''")
		var filterClause string
		if !strings.HasPrefix(sanitizedMatchStr, "*") && !strings.HasSuffix(sanitizedMatchStr, "*") && strings.Count(sanitizedMatchStr, "*") == 1 {
			parts := strings.SplitN(sanitizedMatchStr, "*", 2)
			filterClause = fmt.Sprintf("startsWith(tolower(displayName), '%s') and endsWith(tolower(displayName), '%s')", strings.ToLower(parts[0]), strings.ToLower(parts[1]))
		} else if strings.HasPrefix(sanitizedMatchStr, "*") && strings.HasSuffix(sanitizedMatchStr, "*") {
			filterClause = fmt.Sprintf("contains(tolower(displayName), '%s')", strings.ToLower(strings.Trim(sanitizedMatchStr, "*")))
		} else if strings.HasSuffix(sanitizedMatchStr, "*") {
			filterClause = fmt.Sprintf("startsWith(tolower(displayName), '%s')", strings.ToLower(strings.TrimSuffix(sanitizedMatchStr, "*")))
		} else if strings.HasPrefix(sanitizedMatchStr, "*") {
			filterClause = fmt.Sprintf("endsWith(tolower(displayName), '%s')", strings.ToLower(strings.TrimPrefix(sanitizedMatchStr, "*")))
		} else {
			filterClause = fmt.Sprintf("contains(tolower(displayName), '%s')", strings.ToLower(sanitizedMatchStr))
		}
		filter = strPtr(filterClause)
	}

	// Unlike the main Get() request, the Count() request takes the filter directly in its query parameters.
	requestConfiguration.QueryParameters = &groups.CountRequestBuilderGetQueryParameters{
		Filter: filter,
	}

	count, err := e.client.Groups().Count().Get(e.ctx, requestConfiguration)
	if err != nil {
		return 0, err
	}
	return *count, nil
}

func (e *Extractor) getGroupIterator() (*msgraphgocore.PageIterator[*models.Group], error) {
	initialResult, err := e.getGroupsWithLoginRetry()
	if err != nil {
		return nil, fmt.Errorf("failed to get initial group page: %w", err)
	}
	result, ok := initialResult.(*models.GroupCollectionResponse)
	if !ok {
		return nil, errors.New("could not perform type assertion on the Graph API result")
	}
	return msgraphgocore.NewPageIterator[*models.Group](result, e.client.GetAdapter(), models.CreateGroupCollectionResponseFromDiscriminatorValue)
}

func (e *Extractor) streamJsonToFile(wg *sync.WaitGroup, results <-chan JSONGroup) {
	defer wg.Done()
	file, err := os.Create(e.config.JsonOutputFile)
	if err != nil {
		log.Printf("Error creating JSON output file: %v", err)
		return
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	defer writer.Flush() // Important: ensure buffer is written at the end

	if _, err := writer.WriteString("[\n"); err != nil {
		log.Printf("Error writing opening bracket to JSON file: %v", err)
		return
	}

	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")

	isFirst := true
	for res := range results {
		if !isFirst {
			if _, err := writer.WriteString(",\n"); err != nil {
				log.Printf("Error writing comma to JSON file: %v", err)
				continue
			}
		}
		isFirst = false
		if err := encoder.Encode(res); err != nil {
			log.Printf("Error encoding JSON for group %s: %v", res.ADGroupName, err)
		}
	}

	if _, err := writer.WriteString("\n]"); err != nil {
		log.Printf("Error writing closing bracket to JSON file: %v", err)
	}
	log.Printf("Successfully wrote JSON output to %s", e.config.JsonOutputFile)
}

func (e *Extractor) processSQLiteInserts(wg *sync.WaitGroup, results <-chan SQLiteGroupMember) {
	defer wg.Done()
	tx, err := e.db.BeginTx(e.ctx, nil)
	if err != nil {
		log.Printf("Error starting SQLite transaction: %v", err)
		return
	}
	defer func() {
		if err := tx.Rollback(); err != nil {
			log.Printf("ERROR: transaction rollback failed: %v", err)
		}
	}()

	stmt, err := tx.PrepareContext(e.ctx, "INSERT INTO entraGroups (groupName, groupMember) VALUES (?, ?)")
	if err != nil {
		log.Printf("Error preparing SQLite statement: %v", err)
		return
	}
	defer stmt.Close()

	insertCount := 0
	for res := range results {
		if _, err := stmt.ExecContext(e.ctx, res.GroupName, res.MemberName); err != nil {
			log.Printf("Error executing SQLite insert for group '%s': %v", res.GroupName, err)
			return // Abort on first error to ensure transaction rollback
		}
		insertCount++
	}

	if err := tx.Commit(); err != nil {
		log.Printf("Error committing SQLite transaction: %v", err)
	} else {
		log.Printf("Committed %d records to SQLite.", insertCount)
	}
}

func (e *Extractor) processMembers(members []models.DirectoryObjectable) ([]JSONMember, []SQLiteUser, []string) {
	var jsonMembers []JSONMember
	var sqliteUsers []SQLiteUser
	var groupMemberLinks []string

	for _, member := range members {
		switch m := member.(type) {
		case models.Userable:
			upn := ""
			if val := m.GetUserPrincipalName(); val != nil {
				upn = *val
			}
			if upn == "" {
				continue // Skip users without a UPN
			}

			givenName := ""
			if val := m.GetGivenName(); val != nil {
				givenName = *val
			}
			mail := ""
			if val := m.GetMail(); val != nil {
				mail = *val
			}
			surname := ""
			if val := m.GetSurname(); val != nil {
				surname = *val
			}

			jsonMembers = append(jsonMembers, JSONMember{
				GivenName:         givenName,
				Mail:              mail,
				Surname:           surname,
				UserPrincipalName: upn,
			})
			sqliteUsers = append(sqliteUsers, SQLiteUser{
				UserPrincipalName: upn,
				GivenName:         givenName,
				Mail:              mail,
				Surname:           surname,
			})
			groupMemberLinks = append(groupMemberLinks, upn)

			// Non-user members are ignored for detailed output, only their names are logged for group mapping if needed.
			// case models.Groupable:
			// case models.ServicePrincipalable:
		}
	}
	return jsonMembers, sqliteUsers, groupMemberLinks
}

func (e *Extractor) worker(wg *sync.WaitGroup, groupTasks <-chan *models.Group, jsonResults chan<- JSONGroup, sqliteResults chan<- SQLiteGroupMember, userTasks chan<- SQLiteUser) {
	defer wg.Done()
	for group := range groupTasks {
		if group == nil || group.GetId() == nil || group.GetDisplayName() == nil {
			log.Println("Warning: received a nil group or group with nil ID/DisplayName. Skipping.")
			continue
		}
		groupName := *group.GetDisplayName()

		currentCount := e.processedCount.Add(1)
		if e.countIsReady.Load() {
			total := e.totalGroups.Load()
			if total > 0 {
				percentage := (float64(currentCount) / float64(total)) * 100
				log.Printf("[%d/%d] Data extraction for group %s [%.2f%%]", currentCount, total, groupName, percentage)
			} else {
				log.Printf("[Processed: %d] Data extraction for group %s", currentCount, groupName)
			}
		} else {
			log.Printf("[Processed: %d] Data extraction for group %s", currentCount, groupName)
		}

		var allJsonMembers []JSONMember

		processAndDispatch := func(members []models.DirectoryObjectable) {
			jsonMembers, sqliteUsers, groupMemberLinks := e.processMembers(members)
			allJsonMembers = append(allJsonMembers, jsonMembers...)
			for _, user := range sqliteUsers {
				userTasks <- user
			}
			for _, upn := range groupMemberLinks {
				sqliteResults <- SQLiteGroupMember{GroupName: groupName, MemberName: upn}
			}
		}

		if group.GetMembers() != nil {
			processAndDispatch(group.GetMembers())
		}

		var nextLink *string
		if val, ok := group.GetAdditionalData()["members@odata.nextLink"]; ok && val != nil {
			if s, ok := val.(*string); ok {
				nextLink = s
			}
		}

		if nextLink != nil && *nextLink != "" {
			fakeCollection := models.NewDirectoryObjectCollectionResponse()
			fakeCollection.SetOdataNextLink(nextLink)
			pageIterator, err := msgraphgocore.NewPageIterator[models.DirectoryObjectable](fakeCollection, e.client.GetAdapter(), models.CreateDirectoryObjectCollectionResponseFromDiscriminatorValue)
			if err != nil {
				log.Printf("Error creating page iterator for group %s members nextLink: %v", groupName, err)
			} else {
				err := pageIterator.Iterate(e.ctx, func(member models.DirectoryObjectable) bool {
					if err := e.limiter.Wait(e.ctx); err != nil {
						log.Printf("Error waiting for rate limiter while paginating members for group %s: %v", groupName, err)
						return false
					}
					processAndDispatch([]models.DirectoryObjectable{member})
					return true
				})
				if err != nil {
					log.Printf("Error iterating members for group %s: %v", groupName, err)
				}
			}
		}

		jsonResults <- JSONGroup{ADGroupName: groupName, ADGroupMemberName: allJsonMembers}
	}
}

func (e *Extractor) getGroupsWithLoginRetry() (models.GroupCollectionResponseable, error) {
	requestParameters := &groups.GroupsRequestBuilderGetQueryParameters{
		Select:  []string{"displayName", "id"},
		Expand:  []string{fmt.Sprintf("members($select=givenName,mail,surname,userPrincipalName;$top=%d)", e.config.PageSize)},
		Orderby: []string{"displayName asc"},
		Top:     int32Ptr(e.config.PageSize),
	}

	// Add filter based on provided flags
	if e.config.GroupName != "" {
		// Exact match for one or more group names
		groupNames := strings.Split(e.config.GroupName, ",")
		var filterClauses []string
		for _, name := range groupNames {
			trimmedName := strings.TrimSpace(name)
			if trimmedName != "" {
				sanitizedName := strings.ReplaceAll(trimmedName, "'", "''")
				filterClauses = append(filterClauses, fmt.Sprintf("displayName eq '%s'", sanitizedName))
			}
		}
		if len(filterClauses) > 0 {
			requestParameters.Filter = strPtr(strings.Join(filterClauses, " or "))
			log.Printf("Filtering for groups with exact names: %s", e.config.GroupName)
		}
	} else if e.config.GroupMatch != "" {
		// Partial match using startsWith, endsWith, contains, or a combination
		matchStr := strings.TrimSpace(e.config.GroupMatch)
		sanitizedMatchStr := strings.ReplaceAll(matchStr, "'", "''")

		var filter string
		var matchType string

		if !strings.HasPrefix(sanitizedMatchStr, "*") && !strings.HasSuffix(sanitizedMatchStr, "*") && strings.Count(sanitizedMatchStr, "*") == 1 {
			matchType = "starting and ending with"
			parts := strings.SplitN(sanitizedMatchStr, "*", 2)
			startsWith := parts[0]
			endsWith := parts[1]
			filter = fmt.Sprintf("startsWith(tolower(displayName), '%s') and endsWith(tolower(displayName), '%s')", strings.ToLower(startsWith), strings.ToLower(endsWith))
		} else if strings.HasPrefix(sanitizedMatchStr, "*") && strings.HasSuffix(sanitizedMatchStr, "*") {
			matchType = "containing"
			filter = fmt.Sprintf("contains(tolower(displayName), '%s')", strings.ToLower(strings.Trim(sanitizedMatchStr, "*")))
		} else if strings.HasSuffix(sanitizedMatchStr, "*") {
			matchType = "starting with"
			filter = fmt.Sprintf("startsWith(tolower(displayName), '%s')", strings.ToLower(strings.TrimSuffix(sanitizedMatchStr, "*")))
		} else if strings.HasPrefix(sanitizedMatchStr, "*") {
			matchType = "ending with"
			filter = fmt.Sprintf("endsWith(tolower(displayName), '%s')", strings.ToLower(strings.TrimPrefix(sanitizedMatchStr, "*")))
		} else {
			// Default to contains if no wildcards
			matchType = "containing"
			filter = fmt.Sprintf("contains(tolower(displayName), '%s')", strings.ToLower(sanitizedMatchStr))
		}
		requestParameters.Filter = strPtr(filter)
		log.Printf("Filtering for groups %s: '%s'", matchType, matchStr)
	}

	options := &groups.GroupsRequestBuilderGetRequestConfiguration{
		QueryParameters: requestParameters,
		Headers:         abstractions.NewRequestHeaders(),
	}
	// Required for advanced queries like $filter on displayName and $count
	options.Headers.Add("ConsistencyLevel", "eventual")

	result, err := e.client.Groups().Get(e.ctx, options)
	if err != nil {
		var authFailedErr *azidentity.AuthenticationFailedError
		if errors.As(err, &authFailedErr) {
			fmt.Fprintln(os.Stderr, "Warning: Authentication failed. Attempting to run 'az login'...")
			cmd := exec.Command("az", "login")
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if runErr := cmd.Run(); runErr != nil {
				return nil, fmt.Errorf("failed to run 'az login': %w", runErr)
			}
			fmt.Fprintln(os.Stderr, "Login successful. Retrying API call...")
			return e.client.Groups().Get(e.ctx, options)
		}
		return nil, fmt.Errorf("getting groups failed: %w", err)
	}
	return result, nil
}

// --- Helper functions that do not depend on Extractor state ---

func int32Ptr(i int) *int32 {
	v := int32(i)
	return &v
}

func strPtr(s string) *string {
	return &s
}

func getTenantID(ctx context.Context, cred *azidentity.AzureCLICredential) (string, error) {
	token, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{"https://graph.microsoft.com/.default"}})
	if err != nil {
		return "", fmt.Errorf("failed to get token: %w", err)
	}
	parser := new(jwt.Parser)
	claims := jwt.MapClaims{}
	// Note: We use ParseUnverified because we don't need to validate the token's signature.
	// We are only extracting the tenant ID claim ("tid") from a token that we have just
	// received directly from Azure AD, which we trust as the source.
	// This is NOT safe for authenticating incoming requests.
	_, _, err = parser.ParseUnverified(token.Token, claims)
	if err != nil {
		return "", fmt.Errorf("failed to parse token: %w", err)
	}
	tid, ok := claims["tid"].(string)
	if !ok {
		return "", errors.New("could not find 'tid' claim in token")
	}
	return tid, nil
}

func setupDatabase(ctx context.Context, dbName string) (*sql.DB, error) {
	// Add pragma for performance, accepting the risk of DB corruption on crash.
	dsn := fmt.Sprintf("file:%s?_pragma=synchronous(OFF)", dbName)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	createTableSQL := `CREATE TABLE IF NOT EXISTS entraGroups (groupName TEXT, groupMember TEXT);`
	if _, err := db.ExecContext(ctx, createTableSQL); err != nil {
		return nil, fmt.Errorf("failed to create table: %w", err)
	}
	createGroupNameIndexSQL := `CREATE INDEX IF NOT EXISTS idx_groupName ON entraGroups (groupName);`
	if _, err := db.ExecContext(ctx, createGroupNameIndexSQL); err != nil {
		return nil, fmt.Errorf("failed to create groupName index: %w", err)
	}
	createGroupMemberIndexSQL := `CREATE INDEX IF NOT EXISTS idx_groupMember ON entraGroups (groupMember);`
	if _, err := db.ExecContext(ctx, createGroupMemberIndexSQL); err != nil {
		return nil, fmt.Errorf("failed to create groupMember index: %w", err)
	}

	// New table for user details
	createUsersTableSQL := `CREATE TABLE IF NOT EXISTS entraUsers (UserPrincipalName TEXT PRIMARY KEY, givenName TEXT, mail TEXT, surname TEXT);`
	if _, err := db.ExecContext(ctx, createUsersTableSQL); err != nil {
		return nil, fmt.Errorf("failed to create entraUsers table: %w", err)
	}
	createUserIndexSQL := `CREATE UNIQUE INDEX IF NOT EXISTS idx_userPrincipalName ON entraUsers (UserPrincipalName);`
	if _, err := db.ExecContext(ctx, createUserIndexSQL); err != nil {
		return nil, fmt.Errorf("failed to create userPrincipalName index: %w", err)
	}

	log.Printf("Database '%s' and tables 'entraGroups', 'entraUsers' are set up successfully.", dbName)
	return db, nil
}

func (e *Extractor) processUserInserts(wg *sync.WaitGroup, users <-chan SQLiteUser) {
	defer wg.Done()
	tx, err := e.db.BeginTx(e.ctx, nil)
	if err != nil {
		log.Printf("Error starting SQLite transaction for users: %v", err)
		return
	}
	defer func() {
		if err := tx.Rollback(); err != nil {
			log.Printf("ERROR: transaction rollback failed: %v", err)
		}
	}()

	stmt, err := tx.PrepareContext(e.ctx, "INSERT OR IGNORE INTO entraUsers (UserPrincipalName, givenName, mail, surname) VALUES (?, ?, ?, ?)")
	if err != nil {
		log.Printf("Error preparing SQLite user statement: %v", err)
		return
	}
	defer stmt.Close()

	insertCount := 0
	for user := range users {
		if _, err := stmt.ExecContext(e.ctx, user.UserPrincipalName, user.GivenName, user.Mail, user.Surname); err != nil {
			log.Printf("Error executing SQLite insert for user '%s': %v", user.UserPrincipalName, err)
			return
		}
		insertCount++
	}

	if err := tx.Commit(); err != nil {
		log.Printf("Error committing SQLite user transaction: %v", err)
	} else {
		log.Printf("Committed %d user records to SQLite.", insertCount)
	}
}

func main() {
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
		// Use the version variable, which should be populated by the build process.
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
			log.Fatalf("Error reading config file: %v", err)
		}
		// We unmarshal into the config struct, which already has default values.
		// Any field present in the JSON will overwrite the default.
		if err := json.Unmarshal(file, &config); err != nil {
			log.Fatalf("Error parsing config file: %v", err)
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

	// Validate flags
	if config.PageSize > 999 || config.PageSize < 1 {
		log.Fatalf("Error: pageSize must be between 1 and 999.")
	}
	if config.GroupName != "" && config.GroupMatch != "" {
		log.Fatalf("Error: --group-name and --group-match cannot be used at the same time.")
	}

	// --- Application Setup ---
	ctx := context.Background()

	// Authenticate
	cred, err := azidentity.NewAzureCLICredential(nil)
	if err != nil {
		log.Fatalf("Error creating credential: %v", err)
	}

	// Create Graph client
	client, err := msgraphsdk.NewGraphServiceClientWithCredentials(cred, nil)
	if err != nil {
		log.Fatalf("Error creating Graph client: %v", err)
	}

	// Get Tenant ID for DB naming
	tenantID, err := getTenantID(ctx, cred)
	if err != nil {
		log.Fatalf("Error getting tenant ID: %v", err)
	}

	// Determine base filename
	var baseName string
	if config.OutputID != "" {
		baseName = config.OutputID
	} else {
		baseName = fmt.Sprintf("%s_%s", tenantID, time.Now().Format("20060102-150405"))
	}

	dbName := baseName + ".db"
	config.JsonOutputFile = baseName + ".json"

	// Setup SQLite Database
	db, err := setupDatabase(ctx, dbName)
	if err != nil {
		log.Fatalf("Error setting up database: %v", err)
	}

	// Create and run the extractor
	extractor, err := NewExtractor(ctx, config, client, db, tenantID)
	if err != nil {
		log.Fatalf("Failed to initialize extractor: %v", err)
	}
	if err := extractor.Run(); err != nil {
		log.Fatalf("Application failed: %v", err)
	}
}
