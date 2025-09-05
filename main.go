package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	_ "github.com/glebarez/sqlite"
	"github.com/golang-jwt/jwt/v5"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	msgraphgocore "github.com/microsoftgraph/msgraph-sdk-go-core"
	"github.com/microsoftgraph/msgraph-sdk-go/groups"
	"github.com/microsoftgraph/msgraph-sdk-go/models"
)

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

func main() {
	pageSize := flag.Int("pageSize", 750, "The number of groups to retrieve per page. Max is 999.")
	jsonOutputFile := flag.String("output-file", "adgroupmembers.json", "The path to the output JSON file.")
	flag.Parse()

	if *pageSize > 999 || *pageSize < 1 {
		fmt.Fprintln(os.Stderr, "Error: pageSize must be between 1 and 999.")
		os.Exit(1)
	}

	ctx := context.Background()

	// 1. Set up Azure CLI credentials.
	// This credential type uses the user's Azure CLI login session.
	// Run `az login` to authenticate with the Azure CLI.
	cred, err := azidentity.NewAzureCLICredential(nil)
	if err != nil {
		log.Fatalf("Error creating credential: %v", err)
	}

	// Get Tenant ID for DB naming
	tenantID, err := getTenantID(ctx, cred)
	if err != nil {
		log.Fatalf("Error getting tenant ID: %v", err)
	}
	dbName := fmt.Sprintf("%s.db", tenantID)

	// Setup SQLite Database
	db, err := setupDatabase(ctx, dbName)
	if err != nil {
		log.Fatalf("Error setting up database: %v", err)
	}
	defer db.Close()

	// 2. Create the Microsoft Graph client
	// This is the new, correct way to create the client in recent SDK versions.
	client, err := msgraphsdk.NewGraphServiceClientWithCredentials(cred, nil)
	if err != nil {
		log.Fatalf("Error creating Graph client: %v", err)
	}

	// 3. Make the initial API call to list groups.
	initialResult, err := getGroupsWithLoginRetry(ctx, client, *pageSize)
	if err != nil {
		log.Fatalf("Error: %v", err)
	}

	// The PageIterator needs a concrete type, so we have to type assert the result.
	result, ok := initialResult.(*models.GroupCollectionResponse)
	if !ok {
		fmt.Fprintln(os.Stderr, "Error: could not perform type assertion on the Graph API result.")
		os.Exit(1)
	}

	// 4. Create a Page Iterator to process all pages of results
	pageIterator, err := msgraphgocore.NewPageIterator[*models.Group](result, client.GetAdapter(), models.CreateGroupCollectionResponseFromDiscriminatorValue)
	if err != nil {
		fmt.Printf("Error creating page iterator: %v\n", err)
		os.Exit(1)
	}

	// 5. Setup for parallel processing
	groupTasks := make(chan *models.Group, 100)

	// 6. Iterate over all pages and send groups to the processing channel
	log.Println("Starting to retrieve groups...")
	err = pageIterator.Iterate(ctx, func(group *models.Group) bool {
		groupTasks <- group
		return true // Continue iterating
	})
	if err != nil {
		log.Fatalf("Error during group iteration: %v", err)
	}
	close(groupTasks) // Close channel to signal that no more groups will be sent
	log.Println("Finished retrieving all groups.")

	// 7. Start worker pool for processing groups
	log.Println("Starting group member processing...")
	numWorkers := runtime.NumCPU()
	var wg sync.WaitGroup
	jsonResults := make(chan JSONGroup, 100)
	sqliteResults := make(chan SQLiteGroupMember, 100)

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go worker(ctx, client, &wg, groupTasks, jsonResults, sqliteResults)
	}

	// 8. Start a goroutine to wait for workers and then close the result channels
	go func() {
		wg.Wait()
		close(jsonResults)
		close(sqliteResults)
	}()

	// 9. Process results as they come in
	var aggregationWg sync.WaitGroup

	// SQLite Aggregator
	aggregationWg.Add(1)
	go func() {
		defer aggregationWg.Done()
		if err := processSQLiteInserts(ctx, db, sqliteResults); err != nil {
			log.Printf("Error writing to SQLite database: %v", err)
		}
	}()

	// JSON Aggregator (in main goroutine)
	log.Println("Aggregating results...")
	var finalJsonOutput []JSONGroup
	for res := range jsonResults {
		finalJsonOutput = append(finalJsonOutput, res)
	}

	// Wait for the SQLite aggregator to finish
	aggregationWg.Wait()
	log.Println("Finished aggregating all results.")

	// 10. Write JSON output to file
	// Sort the final results by group name for consistent output
	sort.Slice(finalJsonOutput, func(i, j int) bool {
		return finalJsonOutput[i].ADGroupName < finalJsonOutput[j].ADGroupName
	})

	log.Printf("Writing JSON output to %s...", *jsonOutputFile)
	jsonData, err := json.MarshalIndent(finalJsonOutput, "", "  ")
	if err != nil {
		log.Fatalf("Error marshalling JSON: %v", err)
	}

	err = os.WriteFile(*jsonOutputFile, jsonData, 0644)
	if err != nil {
		log.Fatalf("Error writing JSON to file: %v", err)
	}

	log.Printf("Successfully wrote JSON output to %s", *jsonOutputFile)
}

func processSQLiteInserts(ctx context.Context, db *sql.DB, results <-chan SQLiteGroupMember) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback() // Rollback on error, no-op on success

	stmt, err := tx.PrepareContext(ctx, "INSERT INTO entraGroups (groupName, groupMember) VALUES (?, ?)")
	if err != nil {
		return fmt.Errorf("failed to prepare statement: %w", err)
	}
	defer stmt.Close()

	insertCount := 0
	for res := range results {
		if _, err := stmt.ExecContext(ctx, res.GroupName, res.MemberName); err != nil {
			// This will cause the deferred Rollback to happen
			return fmt.Errorf("failed to execute insert for group '%s': %w", res.GroupName, err)
		}
		insertCount++
	}

	log.Printf("Committing %d records to SQLite...", insertCount)
	return tx.Commit()
}

func worker(ctx context.Context, client *msgraphsdk.GraphServiceClient, wg *sync.WaitGroup, groupTasks <-chan *models.Group, jsonResults chan<- JSONGroup, sqliteResults chan<- SQLiteGroupMember) {
	defer wg.Done()
	for group := range groupTasks {
		// Defensive check: ensure the group and its properties are not nil.
		if group == nil || group.GetId() == nil || group.GetDisplayName() == nil {
			log.Println("Warning: received a nil group or group with nil ID/DisplayName. Skipping.")
			continue
		}
		groupID := *group.GetId()
		groupName := *group.GetDisplayName()

		log.Printf("Fetching members for group: %s", groupName)

		// Fetch members for the group
		result, err := client.Groups().ByGroupId(groupID).Members().Get(ctx, nil)
		if err != nil {
			log.Printf("Error fetching members for group %s (%s): %v", groupName, groupID, err)
			// Send a result with no members so the group is still in the JSON.
			jsonResults <- JSONGroup{ADGroupName: groupName}
			continue
		}

		members := result.GetValue()
		var memberNames []string

		if len(members) > 0 {
			for _, member := range members {
				// The member can be a User, another Group, a Device, etc.
				// We need to perform a type switch to handle them.
				switch m := member.(type) {
				case models.Userable:
					if m.GetDisplayName() != nil {
						memberName := *m.GetDisplayName()
						memberNames = append(memberNames, memberName)
						sqliteResults <- SQLiteGroupMember{GroupName: groupName, MemberName: memberName}
					}
				case models.Groupable:
					if m.GetDisplayName() != nil {
						// Indicate that the member is a nested group.
						memberName := *m.GetDisplayName() + " (Group)"
						memberNames = append(memberNames, memberName)
						sqliteResults <- SQLiteGroupMember{GroupName: groupName, MemberName: memberName}
					}
				case models.ServicePrincipalable:
					if m.GetDisplayName() != nil {
						// Indicate that the member is a service principal
						memberName := *m.GetDisplayName() + " (ServicePrincipal)"
						memberNames = append(memberNames, memberName)
						sqliteResults <- SQLiteGroupMember{GroupName: groupName, MemberName: memberName}
					}
				}
			}
		}

		jsonResults <- JSONGroup{
			ADGroupName:       groupName,
			ADGroupMemberName: memberNames,
		}
	}
}

func getGroupsWithLoginRetry(ctx context.Context, client *msgraphsdk.GraphServiceClient, pageSize int) (models.GroupCollectionResponseable, error) {
	requestParameters := &groups.GroupsRequestBuilderGetQueryParameters{
		Select:  []string{"displayName", "id"},
		Orderby: []string{"displayName asc"},
		Top:     int32Ptr(pageSize),
	}
	options := &groups.GroupsRequestBuilderGetRequestConfiguration{
		QueryParameters: requestParameters,
	}

	result, err := client.Groups().Get(ctx, options)
	if err != nil {
		var authFailedErr *azidentity.AuthenticationFailedError
		if errors.As(err, &authFailedErr) {
			fmt.Fprintln(os.Stderr, "Warning: Authentication failed. This may be because you are not logged into the Azure CLI.")
			fmt.Fprintln(os.Stderr, "Attempting to run 'az login' to re-authenticate...")

			cmd := exec.Command("az", "login")
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr

			if runErr := cmd.Run(); runErr != nil {
				return nil, fmt.Errorf("failed to run 'az login' interactively. Is the Azure CLI installed and in your PATH? Error: %w", runErr)
			}

			fmt.Fprintln(os.Stderr, "Login successful. Retrying API call...")
			return client.Groups().Get(ctx, options)
		}
		return nil, fmt.Errorf("getting groups failed: %w", err)
	}
	return result, nil
}

// int32Ptr returns a pointer to an int32 value.
func int32Ptr(i int) *int32 {
	v := int32(i)
	return &v
}

func getTenantID(ctx context.Context, cred *azidentity.AzureCLICredential) (string, error) {
	// We need a token to get the tenant ID from its claims.
	// The scope determines what the token can be used for.
	// For getting tenant info, a default scope is sufficient.
	token, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{"https://graph.microsoft.com/.default"}})
	if err != nil {
		return "", fmt.Errorf("failed to get token: %w", err)
	}

	// Parse the token without verifying the signature.
	// We trust the token since we just got it from the Azure SDK.
	parser := new(jwt.Parser)
	claims := jwt.MapClaims{}
	_, _, err = parser.ParseUnverified(token.Token, claims)
	if err != nil {
		return "", fmt.Errorf("failed to parse token: %w", err)
	}

	// The tenant ID is in the "tid" claim.
	tid, ok := claims["tid"].(string)
	if !ok {
		return "", fmt.Errorf("could not find 'tid' claim in token")
	}

	return tid, nil
}

func setupDatabase(ctx context.Context, dbName string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbName)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Create the table if it doesn't exist.
	createTableSQL := `
	CREATE TABLE IF NOT EXISTS entraGroups (
		groupName TEXT,
		groupMember TEXT
	);`
	if _, err := db.ExecContext(ctx, createTableSQL); err != nil {
		return nil, fmt.Errorf("failed to create table: %w", err)
	}

	// Create indexes to optimize queries.
	// Using "IF NOT EXISTS" is good practice.
	createGroupNameIndexSQL := "CREATE INDEX IF NOT EXISTS idx_groupName ON entraGroups (groupName);"
	if _, err := db.ExecContext(ctx, createGroupNameIndexSQL); err != nil {
		return nil, fmt.Errorf("failed to create groupName index: %w", err)
	}

	createGroupMemberIndexSQL := "CREATE INDEX IF NOT EXISTS idx_groupMember ON entraGroups (groupMember);"
	if _, err := db.ExecContext(ctx, createGroupMemberIndexSQL); err != nil {
		return nil, fmt.Errorf("failed to create groupMember index: %w", err)
	}

	log.Printf("Database '%s' and table 'entraGroups' are set up successfully.", dbName)

	return db, nil
}
