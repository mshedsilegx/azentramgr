package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/ioutil"
	"os"
	"sync"
	"testing"

	"github.com/microsoftgraph/msgraph-sdk-go/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// MockGroup is a simple struct to hold group info for the mock
type MockGroup struct {
	ID          string
	DisplayName string
	Members     []models.DirectoryObjectable
}

// MockGraphClient is a mock of the GraphServiceClient for testing purposes.
type MockGraphClient struct {
	Groups map[string]MockGroup
}

// ByGroupId is a mock method
func (m *MockGraphClient) ByGroupId(id string) *MockGroupRequestBuilder {
	return &MockGroupRequestBuilder{
		client:  m,
		groupID: id,
	}
}

// MockGroupRequestBuilder is a mock
type MockGroupRequestBuilder struct {
	client  *MockGraphClient
	groupID string
}

// Members is a mock method
func (m *MockGroupRequestBuilder) Members() *MockMembersRequestBuilder {
	return &MockMembersRequestBuilder{
		client:  m.client,
		groupID: m.groupID,
	}
}

// MockMembersRequestBuilder is a mock
type MockMembersRequestBuilder struct {
	client  *MockGraphClient
	groupID string
}

// Get is a mock method
func (m *MockMembersRequestBuilder) Get(ctx context.Context, options any) (models.DirectoryObjectCollectionResponseable, error) {
	group, ok := m.client.Groups[m.groupID]
	if !ok {
		return nil, os.ErrNotExist
	}
	response := models.NewDirectoryObjectCollectionResponse()
	response.SetValue(group.Members)
	return response, nil
}

func TestSpecialCharacterHandling(t *testing.T) {
	// 1. Define test data with special characters
	specialGroupName := `Group Name with "Quotes" & 'Apostrophes' \/`
	specialMemberName := `Member Name with <tags> and /slashes/`
	groupID := "test-group-id"

	// 2. Setup Mock Graph Client
	mockUser := models.NewUser()
	mockUser.SetDisplayName(&specialMemberName)
	mockClient := &MockGraphClient{
		Groups: map[string]MockGroup{
			groupID: {
				ID:          groupID,
				DisplayName: specialGroupName,
				Members:     []models.DirectoryObjectable{mockUser},
			},
		},
	}

	// 3. Setup a temporary database for the test
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE entraGroups (groupName TEXT, groupMember TEXT);`)
	require.NoError(t, err)

	// 4. Setup a temporary JSON file
	tmpFile, err := ioutil.TempFile("", "test-output-*.json")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())
	tmpFile.Close() // Close it so the extractor can open it

	// 5. Setup a minimal Extractor for the test
	// We are not testing the full pipeline, just the worker's processing part.
	extractor := &Extractor{
		db:     db,
		ctx:    context.Background(),
		config: Config{JsonOutputFile: tmpFile.Name()},
	}

	// 6. Create channels and waitgroups to run the processing logic
	jsonResults := make(chan JSONGroup, 1)
	sqliteResults := make(chan SQLiteGroupMember, 1)
	var workerWg sync.WaitGroup
	var aggregatorsWg sync.WaitGroup

	// Mock the client part of the worker. We need a way to inject the mock.
	// For this test, we'll simplify and call the processing logic directly
	// instead of setting up a full mock client in the extractor.
	// This means we will simulate what the worker does with the data.

	// Let's create a fake worker function that uses our mock client logic
	worker := func(wg *sync.WaitGroup, group *models.Group, jsonCh chan<- JSONGroup, sqliteCh chan<- SQLiteGroupMember) {
		defer wg.Done()
		groupName := *group.GetDisplayName()
		groupID := *group.GetId()

		// Mocked API call
		result, err := mockClient.ByGroupId(groupID).Members().Get(context.Background(), nil)
		require.NoError(t, err)

		members := result.GetValue()
		var memberNames []string
		for _, member := range members {
			if u, ok := member.(models.Userable); ok {
				if u.GetDisplayName() != nil {
					memberName := *u.GetDisplayName()
					memberNames = append(memberNames, memberName)
					sqliteCh <- SQLiteGroupMember{GroupName: groupName, MemberName: memberName}
				}
			}
		}
		jsonCh <- JSONGroup{ADGroupName: groupName, ADGroupMemberName: memberNames}
	}

	// Start the aggregators
	aggregatorsWg.Add(1)
	go extractor.streamJsonToFile(&aggregatorsWg, jsonResults)
	aggregatorsWg.Add(1)
	go extractor.processSQLiteInserts(&aggregatorsWg, sqliteResults)

	// Start the worker
	workerWg.Add(1)
	testGroup := models.NewGroup()
	testGroup.SetId(&groupID)
	testGroup.SetDisplayName(&specialGroupName)
	go worker(&workerWg, testGroup, jsonResults, sqliteResults)

	// Close channels once worker is done
	go func() {
		workerWg.Wait()
		close(jsonResults)
		close(sqliteResults)
	}()

	// Wait for aggregators to finish
	aggregatorsWg.Wait()

	// 7. Verification
	// Verify JSON output
	jsonContent, err := ioutil.ReadFile(extractor.config.JsonOutputFile)
	require.NoError(t, err)

	var outputGroups []JSONGroup
	// We need to unmarshal the content between the brackets
	err = json.Unmarshal(jsonContent[1:len(jsonContent)-1], &outputGroups)
	// A bit of a hack because streamJsonToFile writes `[\n` and `\n]`
	// A single object will be written as `[\n { ... } \n]`
	// Let's just unmarshal the inner part.
	var singleGroup JSONGroup
	// Find the first `{` and last `}`
	start := -1
	end := -1
	for i, b := range jsonContent {
		if b == '{' && start == -1 {
			start = i
		}
		if b == '}' {
			end = i
		}
	}
	require.NotEqual(t, -1, start, "Could not find opening brace in JSON output")
	require.NotEqual(t, -1, end, "Could not find closing brace in JSON output")

	err = json.Unmarshal(jsonContent[start:end+1], &singleGroup)
	require.NoError(t, err, "Failed to unmarshal JSON: %s", string(jsonContent))

	assert.Equal(t, specialGroupName, singleGroup.ADGroupName)
	require.Len(t, singleGroup.ADGroupMemberName, 1)
	assert.Equal(t, specialMemberName, singleGroup.ADGroupMemberName[0])

	// Verify SQLite content
	var dbGroupName, dbMemberName string
	err = db.QueryRow("SELECT groupName, groupMember FROM entraGroups").Scan(&dbGroupName, &dbMemberName)
	require.NoError(t, err)
	assert.Equal(t, specialGroupName, dbGroupName)
	assert.Equal(t, specialMemberName, dbMemberName)
}
