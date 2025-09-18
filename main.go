package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	_ "github.com/glebarez/sqlite"
	abstractions "github.com/microsoft/kiota-abstractions-go"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	msgraphgocore "github.com/microsoftgraph/msgraph-sdk-go-core"
	"github.com/microsoftgraph/msgraph-sdk-go/groups"
	"github.com/microsoftgraph/msgraph-sdk-go/models"
	"github.com/microsoftgraph/msgraph-sdk-go/models/odataerrors"
)

var version = "dev"

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
	apiMutex       sync.Mutex
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

// RequestType differentiates between different kinds of database write requests.
type RequestType int

const (
	// UserRequest is for inserting a user.
	UserRequest RequestType = iota
	// GroupMemberRequest is for inserting a group member relationship.
	GroupMemberRequest
)

// DBWriteRequest encapsulates a request to write to the database.
type DBWriteRequest struct {
	Type        RequestType
	User        SQLiteUser
	GroupMember SQLiteGroupMember
}

// Run executes the main logic of the extractor.
func (e *Extractor) Run() error {
	defer func() {
		if err := e.db.Close(); err != nil {
			log.Printf("Error closing database: %v", err)
		}
	}()

	// 1. Get total count of groups for progress reporting.
	// The Graph API's $count endpoint does not support complex filters like startsWith,
	// so we skip the count for partial matches to avoid an "Unsupported Query" error.
	var totalGroupsInTenant int32
	var err error
	if e.config.GroupMatch != "" {
		log.Println("Note: A total count is not available for partial matches. Starting extraction...")
		totalGroupsInTenant = 0 // Set to 0, progress will be shown without a total.
	} else {
		totalGroupsInTenant, err = e.getGroupCount()
		if err != nil {
			return fmt.Errorf("could not get total group count: %w", err)
		}
		if e.config.GroupName != "" {
			log.Printf("Found %d group(s) matching the provided exact name(s). Starting extraction.", totalGroupsInTenant)
		} else {
			log.Printf("Found %d total groups in tenant. Starting full scan...", totalGroupsInTenant)
		}
	}

	// 2. Setup channels and wait groups for concurrent processing
	groupTasks := make(chan *models.Group, 100)
	jsonResults := make(chan JSONGroup, 100)
	dbWriteTasks := make(chan DBWriteRequest, 200) // A single channel for all DB writes
	var workersWg, aggregatorsWg sync.WaitGroup

	// 3. Start workers and result aggregators
	numWorkers := e.config.ParallelJobs
	for i := 0; i < numWorkers; i++ {
		workersWg.Add(1)
		// Pass the new unified DB write channel to the worker
		go e.worker(&workersWg, groupTasks, jsonResults, dbWriteTasks)
	}
	aggregatorsWg.Add(1)
	// Start a single goroutine to process all database writes
	go e.processDBWrites(&aggregatorsWg, dbWriteTasks)
	aggregatorsWg.Add(1)
	go streamJsonToFile(&aggregatorsWg, jsonResults, e.config.JsonOutputFile)

	// 4. Goroutine to close result channels once all workers are done
	go func() {
		workersWg.Wait()
		close(jsonResults)
		close(dbWriteTasks) // Close the single DB write channel
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
			filterClause = fmt.Sprintf("startsWith(displayName, '%s') and endsWith(displayName, '%s')", parts[0], parts[1])
		} else if strings.HasPrefix(sanitizedMatchStr, "*") && strings.HasSuffix(sanitizedMatchStr, "*") {
			filterClause = fmt.Sprintf("contains(displayName, '%s')", strings.Trim(sanitizedMatchStr, "*"))
		} else if strings.HasSuffix(sanitizedMatchStr, "*") {
			filterClause = fmt.Sprintf("startsWith(displayName, '%s')", strings.TrimSuffix(sanitizedMatchStr, "*"))
		} else if strings.HasPrefix(sanitizedMatchStr, "*") {
			filterClause = fmt.Sprintf("endsWith(displayName, '%s')", strings.TrimPrefix(sanitizedMatchStr, "*"))
		} else {
			filterClause = fmt.Sprintf("contains(displayName, '%s')", sanitizedMatchStr)
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

func (e *Extractor) getUser() (string, error) {
	// Request the user's profile.
	// Note: We cannot use $select here as the Get method for the MeRequestBuilder
	// in this SDK version does not appear to support a configuration object.
	user, err := e.client.Me().Get(e.ctx, nil)
	if err != nil {
		return "", fmt.Errorf("failed to get current user: %w", err)
	}

	displayName := user.GetDisplayName()
	if displayName == nil {
		return "N/A", nil // Should not happen in normal circumstances
	}

	return *displayName, nil
}

func streamJsonToFile(wg *sync.WaitGroup, results <-chan JSONGroup, outputFile string) {
	defer wg.Done()
	file, err := os.Create(outputFile)
	if err != nil {
		log.Printf("Error creating JSON output file: %v", err)
		return
	}
	defer func() {
		if err := file.Close(); err != nil {
			log.Printf("Error closing file %s: %v", outputFile, err)
		}
	}()

	writer := bufio.NewWriter(file)
	defer func() {
		if err := writer.Flush(); err != nil {
			log.Printf("Error flushing writer for file %s: %v", outputFile, err)
		}
	}()

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
	log.Printf("Successfully wrote JSON output to %s", outputFile)
}

// processDBWrites handles all database insertions in a single, serialized transaction.
func (e *Extractor) processDBWrites(wg *sync.WaitGroup, requests <-chan DBWriteRequest) {
	defer wg.Done()
	tx, err := e.db.BeginTx(e.ctx, nil)
	if err != nil {
		log.Printf("Error starting combined SQLite transaction: %v", err)
		return
	}
	// Use a deferred function to ensure the transaction is rolled back on any error path.
	defer func() {
		if p := recover(); p != nil {
			if rbErr := tx.Rollback(); rbErr != nil {
				log.Printf("ERROR: transaction rollback failed during panic recovery: %v", rbErr)
			}
			panic(p) // re-throw panic after Rollback
		} else if err != nil {
			if rbErr := tx.Rollback(); rbErr != nil {
				log.Printf("ERROR: transaction rollback failed: %v", rbErr)
			}
		}
	}()

	userStmt, err := tx.PrepareContext(e.ctx, "INSERT OR IGNORE INTO entraUsers (UserPrincipalName, givenName, mail, surname, isEnabled) VALUES (?, ?, ?, ?, ?)")
	if err != nil {
		log.Printf("Error preparing SQLite user statement: %v", err)
		return
	}
	defer func() {
		if err := userStmt.Close(); err != nil {
			log.Printf("Error closing user statement: %v", err)
		}
	}()

	groupStmt, err := tx.PrepareContext(e.ctx, "INSERT INTO entraGroups (groupName, groupMember) VALUES (?, ?)")
	if err != nil {
		log.Printf("Error preparing SQLite group statement: %v", err)
		return
	}
	defer func() {
		if err := groupStmt.Close(); err != nil {
			log.Printf("Error closing group statement: %v", err)
		}
	}()

	userInsertCount := 0
	groupInsertCount := 0

	for req := range requests {
		switch req.Type {
		case UserRequest:
			if _, err = userStmt.ExecContext(e.ctx, req.User.UserPrincipalName, req.User.GivenName, req.User.Mail, req.User.Surname, req.User.IsEnabled); err != nil {
				log.Printf("Error executing SQLite insert for user '%s': %v", req.User.UserPrincipalName, err)
				return // Abort on first error
			}
			userInsertCount++
		case GroupMemberRequest:
			if _, err = groupStmt.ExecContext(e.ctx, req.GroupMember.GroupName, req.GroupMember.MemberName); err != nil {
				log.Printf("Error executing SQLite insert for group '%s': %v", req.GroupMember.GroupName, err)
				return // Abort on first error
			}
			groupInsertCount++
		}
	}

	if err = tx.Commit(); err != nil {
		log.Printf("Error committing combined SQLite transaction: %v", err)
	} else {
		log.Printf("Committed %d user records and %d group membership records to SQLite.", userInsertCount, groupInsertCount)
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
			isEnabled := m.GetAccountEnabled() // This returns *bool

			jsonMembers = append(jsonMembers, JSONMember{
				GivenName:         givenName,
				Mail:              mail,
				Surname:           surname,
				UserPrincipalName: upn,
				IsEnabled:         isEnabled,
			})
			sqliteUsers = append(sqliteUsers, SQLiteUser{
				UserPrincipalName: upn,
				GivenName:         givenName,
				Mail:              mail,
				Surname:           surname,
				IsEnabled:         isEnabled != nil && *isEnabled, // Dereference pointer for SQLite
			})
			groupMemberLinks = append(groupMemberLinks, upn)

			// Non-user members are ignored for detailed output, only their names are logged for group mapping if needed.
			// case models.Groupable:
			// case models.ServicePrincipalable:
		}
	}
	return jsonMembers, sqliteUsers, groupMemberLinks
}

func (e *Extractor) worker(wg *sync.WaitGroup, groupTasks <-chan *models.Group, jsonResults chan<- JSONGroup, dbWriteTasks chan<- DBWriteRequest) {
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

		if e.config.GroupListOnly {
			jsonResults <- JSONGroup{ADGroupName: groupName, ADGroupMemberName: nil}
			continue
		}

		var allJsonMembers []JSONMember

		processAndDispatch := func(members []models.DirectoryObjectable) {
			jsonMembers, sqliteUsers, groupMemberLinks := e.processMembers(members)
			allJsonMembers = append(allJsonMembers, jsonMembers...)
			for _, user := range sqliteUsers {
				dbWriteTasks <- DBWriteRequest{Type: UserRequest, User: user}
			}
			for _, upn := range groupMemberLinks {
				dbWriteTasks <- DBWriteRequest{Type: GroupMemberRequest, GroupMember: SQLiteGroupMember{GroupName: groupName, MemberName: upn}}
			}
		}

		// --- Member Processing Logic ---
		// This block is wrapped in a mutex to prevent race conditions in the
		// underlying Azure CLI credential provider when running with high concurrency.
		// This should only apply to `azidentity` auth.
		memberProcessingFunc := func() {
			var initialMembers []models.DirectoryObjectable
			var nextLink *string

			if group.GetMembers() != nil {
				initialMembers = group.GetMembers()
				if val, ok := group.GetAdditionalData()["members@odata.nextLink"]; ok && val != nil {
					if s, ok := val.(*string); ok {
						nextLink = s
					}
				}
			} else if e.config.GroupMatch != "" {
				// Manually fetch the first page of members
				if err := e.limiter.Wait(e.ctx); err != nil {
					log.Printf("Error waiting for rate limiter while fetching members for group %s: %v", groupName, err)
					return // Exit the func, releasing the lock
				}

				headers := abstractions.NewRequestHeaders()
				headers.Add("ConsistencyLevel", "eventual")
				requestParameters := &groups.ItemMembersRequestBuilderGetQueryParameters{
					Select: []string{"givenName", "mail", "surname", "userPrincipalName", "accountEnabled"},
					Top:    int32Ptr(e.config.PageSize),
				}
				options := &groups.ItemMembersRequestBuilderGetRequestConfiguration{
					Headers:         headers,
					QueryParameters: requestParameters,
				}
				result, err := e.client.Groups().ByGroupId(*group.GetId()).Members().Get(e.ctx, options)
				if err != nil {
					log.Printf("Error manually fetching members for group %s: %v", groupName, err)
				} else {
					initialMembers = result.GetValue()
					nextLink = result.GetOdataNextLink()
				}
			}

			// Process the first page of members
			if initialMembers != nil {
				processAndDispatch(initialMembers)
			}

			// Manually handle subsequent pages if a nextLink exists
			for nextLink != nil && *nextLink != "" {
				if err := e.limiter.Wait(e.ctx); err != nil {
					log.Printf("Error waiting for rate limiter while paginating members for group %s: %v", groupName, err)
					break // Stop paginating for this group on error
				}

				// Build a new request object for the next page URL
				nextPageRequestBuilder := groups.NewItemMembersRequestBuilder(*nextLink, e.client.GetAdapter())
				result, err := nextPageRequestBuilder.Get(e.ctx, nil)
				if err != nil {
					log.Printf("Error fetching next page of members for group %s: %v", groupName, err)
					break // Stop paginating for this group on error
				}

				processAndDispatch(result.GetValue())
				nextLink = result.GetOdataNextLink()
			}
		}

		if e.config.AuthMethod == "azidentity" {
			e.apiMutex.Lock()
			memberProcessingFunc()
			e.apiMutex.Unlock()
		} else {
			memberProcessingFunc()
		}

		jsonResults <- JSONGroup{ADGroupName: groupName, ADGroupMemberName: allJsonMembers}
	}
}

func (e *Extractor) getGroupsWithLoginRetry() (models.GroupCollectionResponseable, error) {
	requestParameters := &groups.GroupsRequestBuilderGetQueryParameters{
		Select: []string{"displayName", "id"},
		Top:    int32Ptr(e.config.PageSize),
	}

	// Conditionally add Orderby. Graph API does not support sorting when filtering on displayName.
	if e.config.GroupName == "" && e.config.GroupMatch == "" {
		requestParameters.Orderby = []string{"displayName asc"}
	}

	// Conditionally add Expand. Graph API does not support expanding members when using a partial-text filter.
	if e.config.GroupMatch == "" {
		requestParameters.Expand = []string{fmt.Sprintf("members($select=givenName,mail,surname,userPrincipalName,accountEnabled;$top=%d)", e.config.PageSize)}
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
			filter = fmt.Sprintf("startsWith(displayName, '%s') and endsWith(displayName, '%s')", startsWith, endsWith)
		} else if strings.HasPrefix(sanitizedMatchStr, "*") && strings.HasSuffix(sanitizedMatchStr, "*") {
			matchType = "containing"
			filter = fmt.Sprintf("contains(displayName, '%s')", strings.Trim(sanitizedMatchStr, "*"))
		} else if strings.HasSuffix(sanitizedMatchStr, "*") {
			matchType = "starting with"
			filter = fmt.Sprintf("startsWith(displayName, '%s')", strings.TrimSuffix(sanitizedMatchStr, "*"))
		} else if strings.HasPrefix(sanitizedMatchStr, "*") {
			matchType = "ending with"
			filter = fmt.Sprintf("endsWith(displayName, '%s')", strings.TrimPrefix(sanitizedMatchStr, "*"))
		} else {
			// Default to contains if no wildcards
			matchType = "containing"
			filter = fmt.Sprintf("contains(displayName, '%s')", sanitizedMatchStr)
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

func main() {
	// Load configuration from flags and config file.
	config, err := LoadConfig()
	if err != nil {
		log.Fatal(err)
	}

	if config.HealthCheck {
		runHealthCheck(config)
		return
	}

	if config.UseCache != "" {
		if err := runFromCache(config); err != nil {
			log.Fatalf("Application failed while running from cache: %v", err)
		}
		return
	}

	// --- Application Setup ---
	ctx := context.Background()

	// Authenticate
	var cred azcore.TokenCredential
	var tenantID string

	switch config.AuthMethod {
	case "azidentity":
		if config.TenantIDFlag != "" {
			log.Printf("Using tenant ID from -tenantid flag: %s", config.TenantIDFlag)
			tenantID = config.TenantIDFlag
			opts := &azidentity.AzureCLICredentialOptions{TenantID: tenantID}
			cred, err = azidentity.NewAzureCLICredential(opts)
			if err != nil {
				log.Fatalf("Error creating Azure CLI credential for tenant %s: %v", tenantID, err)
			}
		} else {
			cred, err = azidentity.NewAzureCLICredential(nil)
			if err != nil {
				log.Fatalf("Error creating Azure CLI credential: %v", err)
			}
			// Get Tenant ID for DB naming
			tenantID, err = getTenantID(ctx, cred)
			if err != nil {
				log.Fatalf("Error getting tenant ID: %v", err)
			}
			log.Printf("Auto-detected tenant ID: %s", tenantID)
		}
	case "clientid":
		// The -tenantid flag overrides the tenant ID from the config file/env.
		if config.TenantIDFlag != "" {
			tenantID = config.TenantIDFlag
			log.Printf("Using tenant ID from -tenantid flag: %s (overriding config/env)", tenantID)
		} else {
			tenantID = config.TenantID
		}
		cred, err = azidentity.NewClientSecretCredential(tenantID, config.ClientID, config.ClientSecret, nil)
		if err != nil {
			log.Fatalf("Error creating client secret credential: %v", err)
		}
	default:
		log.Fatalf("Unknown authentication method: %s", config.AuthMethod)
	}

	// Create Graph client
	client, err := msgraphsdk.NewGraphServiceClientWithCredentials(cred, nil)
	if err != nil {
		log.Fatalf("Error creating Graph client: %v", err)
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

func runHealthCheck(config Config) {
	ctx := context.Background()
	var cred azcore.TokenCredential
	var tenantID string
	var err error

	// --- Authentication ---
	switch config.AuthMethod {
	case "azidentity":
		if config.TenantIDFlag != "" {
			tenantID = config.TenantIDFlag
			opts := &azidentity.AzureCLICredentialOptions{TenantID: tenantID}
			cred, err = azidentity.NewAzureCLICredential(opts)
			if err != nil {
				log.Fatalf("Error creating Azure CLI credential for tenant %s: %v", tenantID, err)
			}
		} else {
			cred, err = azidentity.NewAzureCLICredential(nil)
			if err != nil {
				log.Fatalf("Error creating Azure CLI credential: %v", err)
			}
			tenantID, err = getTenantID(ctx, cred)
			if err != nil {
				log.Fatalf("Error getting tenant ID: %v", err)
			}
		}
	case "clientid":
		if config.TenantIDFlag != "" {
			tenantID = config.TenantIDFlag
		} else {
			tenantID = config.TenantID
		}
		cred, err = azidentity.NewClientSecretCredential(tenantID, config.ClientID, config.ClientSecret, nil)
		if err != nil {
			log.Fatalf("Error creating client secret credential: %v", err)
		}
	default:
		log.Fatalf("Unknown authentication method: %s", config.AuthMethod)
	}

	// --- API Calls ---
	client, err := msgraphsdk.NewGraphServiceClientWithCredentials(cred, nil)
	if err != nil {
		log.Fatalf("Error creating Graph client: %v", err)
	}
	extractor, err := NewExtractor(ctx, config, client, nil, tenantID) // DB is nil for healthcheck
	if err != nil {
		log.Fatalf("Failed to initialize extractor: %v", err)
	}

	groupCount, err := extractor.getGroupCount()
	if err != nil {
		var odataErr *odataerrors.ODataError
		if errors.As(err, &odataErr) {
			// Print the main error message. The structure for detailed codes seems to have changed.
			fmt.Fprintf(os.Stderr, "API Error: %s\n", odataErr.Error())
		} else {
			fmt.Fprintf(os.Stderr, "Error getting group count: %v\n", err)
		}
		os.Exit(1)
	}

	userName, err := extractor.getUser()
	if err != nil {
		var odataErr *odataerrors.ODataError
		if errors.As(err, &odataErr) {
			// Print the main error message.
			fmt.Fprintf(os.Stderr, "API Error: %s\n", odataErr.Error())
		} else {
			fmt.Fprintf(os.Stderr, "Error getting user: %v\n", err)
		}
		os.Exit(1)
	}

	// --- Success ---
	result := map[string]interface{}{
		"groupCount":  groupCount,
		"tenantId":    tenantID,
		"currentUser": userName,
	}
	jsonResult, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(jsonResult))
}
