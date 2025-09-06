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
	"regexp"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	_ "github.com/glebarez/sqlite"
	"github.com/golang-jwt/jwt/v5"
	"github.com/microsoft/kiota-abstractions-go"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	msgraphgocore "github.com/microsoftgraph/msgraph-sdk-go-core"
	"github.com/microsoftgraph/msgraph-sdk-go/groups"
	"github.com/microsoftgraph/msgraph-sdk-go/models"
)

var version = "dev"

// JSONGroup represents the structure for each group in the final JSON output.
type JSONGroup struct {
	ADGroupName       string   `json:"ADGroupName"`
	ADGroupMemberName []string `json:"ADGroupMemberName,omitempty"` // Use omitempty to hide if nil/empty
}

// SQLiteGroupMember represents a single row in the SQLite database.
type SQLiteGroupMember struct {
	GroupName  string
	MemberName string
}

// Config holds the configuration options for the extractor.
type Config struct {
	PageSize         int
	OutputID         string
	GroupFilterRegex string
	JsonOutputFile   string // The full path to the JSON output file
}

// Extractor holds the application's state and logic.
type Extractor struct {
	config         Config
	ctx            context.Context
	client         *msgraphsdk.GraphServiceClient
	db             *sql.DB
	limiter        *rate.Limiter
	groupFilter    *regexp.Regexp
	tenantID       string
	totalGroups    atomic.Int64
	processedCount atomic.Int64
	countIsReady   atomic.Bool
}

// NewExtractor creates and initializes a new Extractor.
func NewExtractor(ctx context.Context, config Config, client *msgraphsdk.GraphServiceClient, db *sql.DB, tenantID string) (*Extractor, error) {
	// Compile regex
	var groupFilter *regexp.Regexp
	if config.GroupFilterRegex != "" {
		var err error
		groupFilter, err = regexp.Compile(config.GroupFilterRegex)
		if err != nil {
			return nil, fmt.Errorf("error compiling group filter regex: %w", err)
		}
		log.Printf("Filtering groups by regex: %s", config.GroupFilterRegex)
	}

	return &Extractor{
		config:      config,
		ctx:         ctx,
		client:      client,
		db:          db,
		limiter:     rate.NewLimiter(rate.Limit(15), 1),
		groupFilter: groupFilter,
		tenantID:    tenantID,
	}, nil
}

// Run executes the main logic of the extractor.
func (e *Extractor) Run() error {
	defer e.db.Close()

	// 1. Get total count of ALL groups for progress reporting.
	// This is an estimate if a filter is used.
	totalGroupsInTenant, err := e.getGroupCount()
	if err != nil {
		return fmt.Errorf("could not get total group count: %w", err)
	}
	log.Printf("Found %d total groups in tenant. Starting scan...", totalGroupsInTenant)

	// 2. Setup channels and wait groups for concurrent processing
	groupTasks := make(chan *models.Group, 100)
	jsonResults := make(chan JSONGroup, 100)
	sqliteResults := make(chan SQLiteGroupMember, 100)
	var workersWg, aggregatorsWg sync.WaitGroup

	// 3. Start workers and result aggregators
	numWorkers := runtime.NumCPU()
	for i := 0; i < numWorkers; i++ {
		workersWg.Add(1)
		go e.worker(&workersWg, groupTasks, jsonResults, sqliteResults)
	}
	aggregatorsWg.Add(1)
	go e.processSQLiteInserts(&aggregatorsWg, sqliteResults)
	aggregatorsWg.Add(1)
	go e.streamJsonToFile(&aggregatorsWg, jsonResults)

	// 4. Goroutine to close result channels once all workers are done
	go func() {
		workersWg.Wait()
		close(jsonResults)
		close(sqliteResults)
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

			if e.groupFilter == nil || (group.GetDisplayName() != nil && e.groupFilter.MatchString(*group.GetDisplayName())) {
				matchingCount++
				groupTasks <- group
			}
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
	requestConfiguration := &groups.CountRequestBuilderGetRequestConfiguration{
		Headers: headers,
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
	defer tx.Rollback()

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

func (e *Extractor) worker(wg *sync.WaitGroup, groupTasks <-chan *models.Group, jsonResults chan<- JSONGroup, sqliteResults chan<- SQLiteGroupMember) {
	defer wg.Done()
	for group := range groupTasks {
		if group == nil || group.GetId() == nil || group.GetDisplayName() == nil {
			log.Println("Warning: received a nil group or group with nil ID/DisplayName. Skipping.")
			continue
		}
		groupID := *group.GetId()
		groupName := *group.GetDisplayName()

		currentCount := e.processedCount.Add(1)
		if e.countIsReady.Load() {
			total := e.totalGroups.Load()
			if total > 0 {
				percentage := (float64(currentCount) / float64(total)) * 100
				log.Printf("[%d/%d] Data extraction for group %s [%.2f%%]", currentCount, total, groupName, percentage)
			} else {
				// Fallback for case where count is ready but zero (e.g., no matching groups)
				log.Printf("[Processed: %d] Data extraction for group %s", currentCount, groupName)
			}
		} else {
			log.Printf("[Processed: %d] Data extraction for group %s", currentCount, groupName)
		}

		if err := e.limiter.Wait(e.ctx); err != nil {
			log.Printf("Error waiting for rate limiter: %v", err)
			jsonResults <- JSONGroup{ADGroupName: groupName}
			continue
		}

		// Fetch the first page of members
		requestParameters := &groups.ItemMembersRequestBuilderGetQueryParameters{
			Top: int32Ptr(e.config.PageSize),
		}
		options := &groups.ItemMembersRequestBuilderGetRequestConfiguration{
			QueryParameters: requestParameters,
		}
		membersCollection, err := e.client.Groups().ByGroupId(groupID).Members().Get(e.ctx, options)
		if err != nil {
			log.Printf("Error fetching members for group %s (%s): %v", groupName, groupID, err)
			jsonResults <- JSONGroup{ADGroupName: groupName} // Send group name even if member fetch fails
			continue
		}

		var memberNames []string
		// Create a PageIterator for the members
		memberIterator, err := msgraphgocore.NewPageIterator[models.DirectoryObjectable](membersCollection, e.client.GetAdapter(), models.CreateDirectoryObjectCollectionResponseFromDiscriminatorValue)
		if err != nil {
			log.Printf("Error creating member iterator for group %s (%s): %v", groupName, groupID, err)
			jsonResults <- JSONGroup{ADGroupName: groupName}
			continue
		}

		// Iterate over all pages of members
		err = memberIterator.Iterate(e.ctx, func(member models.DirectoryObjectable) bool {
			var memberName string
			memberType := ""

			switch m := member.(type) {
			case models.Userable:
				if m.GetDisplayName() != nil {
					memberName = *m.GetDisplayName()
				}
			case models.Groupable:
				if m.GetDisplayName() != nil {
					memberName = *m.GetDisplayName()
					memberType = " (Group)"
				}
			case models.ServicePrincipalable:
				if m.GetDisplayName() != nil {
					memberName = *m.GetDisplayName()
					memberType = " (ServicePrincipal)"
				}
			}

			if memberName != "" {
				fullMemberName := memberName + memberType
				memberNames = append(memberNames, fullMemberName)
				sqliteResults <- SQLiteGroupMember{GroupName: groupName, MemberName: fullMemberName}
			}
			return true // Continue iterating
		})
		if err != nil {
			log.Printf("Error iterating members for group %s (%s): %v", groupName, groupID, err)
			// Still send the group name to the JSON results, even if member iteration fails mid-way
		}

		jsonResults <- JSONGroup{ADGroupName: groupName, ADGroupMemberName: memberNames}
	}
}

func (e *Extractor) getGroupsWithLoginRetry() (models.GroupCollectionResponseable, error) {
	requestParameters := &groups.GroupsRequestBuilderGetQueryParameters{
		Select:  []string{"displayName", "id"},
		Orderby: []string{"displayName asc"},
		Top:     int32Ptr(e.config.PageSize),
	}
	options := &groups.GroupsRequestBuilderGetRequestConfiguration{QueryParameters: requestParameters}

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
	log.Printf("Database '%s' and table 'entraGroups' are set up successfully.", dbName)
	return db, nil
}

func main() {
	// Define and parse flags
	config := Config{}
	versionFlag := flag.Bool("version", false, "Print the version and exit.")
	flag.IntVar(&config.PageSize, "pageSize", 500, "The number of items to retrieve per page for API queries. Max is 999.")
	flag.StringVar(&config.OutputID, "output-id", "", "Custom ID for output filenames (e.g., 'my-export'). If empty, a default ID is generated.")
	flag.StringVar(&config.GroupFilterRegex, "group-filter-regex", "", "Optional regex to filter groups by name. Note: complex patterns can cause performance issues (ReDoS).")
	flag.Parse()

	if *versionFlag {
		fmt.Println(version)
		os.Exit(0)
	}

	// Validate flags
	if config.PageSize > 999 || config.PageSize < 1 {
		log.Fatalf("Error: pageSize must be between 1 and 999.")
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
