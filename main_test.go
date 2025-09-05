package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/ioutil"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSpecialCharacterHandlingInOutput(t *testing.T) {
	// 1. Define test data with special characters
	specialGroupName := `Group Name with "Quotes" & 'Apostrophes' \/`
	specialMemberName := `Member Name with <tags> and /slashes/`

	// 2. Setup in-memory database
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE entraGroups (groupName TEXT, groupMember TEXT);`)
	require.NoError(t, err)

	// 3. Setup temp file for JSON output
	tmpFile, err := ioutil.TempFile("", "test-output-*.json")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())
	tmpFile.Close() // Close so extractor can open it

	// 4. Create a minimal Extractor to test the output functions
	config := Config{JsonOutputFile: tmpFile.Name()}
	extractor := &Extractor{
		config: config,
		ctx:    context.Background(),
		db:     db,
	}

	// 5. Run the aggregator functions with test data
	jsonResults := make(chan JSONGroup, 1)
	sqliteResults := make(chan SQLiteGroupMember, 1)
	var aggregatorsWg sync.WaitGroup

	aggregatorsWg.Add(2)
	go extractor.streamJsonToFile(&aggregatorsWg, jsonResults)
	go extractor.processSQLiteInserts(&aggregatorsWg, sqliteResults)

	// Send test data directly to the aggregator channels
	jsonResults <- JSONGroup{
		ADGroupName:       specialGroupName,
		ADGroupMemberName: []string{specialMemberName},
	}
	sqliteResults <- SQLiteGroupMember{
		GroupName:  specialGroupName,
		MemberName: specialMemberName,
	}
	close(jsonResults)
	close(sqliteResults)

	aggregatorsWg.Wait() // Wait for file and DB writes to complete

	// 6. Verify the output
	// Verify JSON file content
	jsonContent, err := ioutil.ReadFile(tmpFile.Name())
	require.NoError(t, err)

	var outputGroups []JSONGroup
	// The file is a stream `[\n { ... } \n]`, so we unmarshal the inner object
	start, end := -1, -1
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

	var singleGroup JSONGroup
	err = json.Unmarshal(jsonContent[start:end+1], &singleGroup)
	require.NoError(t, err, "Failed to unmarshal JSON: %s", string(jsonContent))
	outputGroups = []JSONGroup{singleGroup}

	require.Len(t, outputGroups, 1)
	assert.Equal(t, specialGroupName, outputGroups[0].ADGroupName)
	require.Len(t, outputGroups[0].ADGroupMemberName, 1)
	assert.Equal(t, specialMemberName, outputGroups[0].ADGroupMemberName[0])

	// Verify SQLite database content
	var dbGroupName, dbMemberName string
	err = db.QueryRow("SELECT groupName, groupMember FROM entraGroups WHERE groupName = ?", specialGroupName).Scan(&dbGroupName, &dbMemberName)
	require.NoError(t, err)
	assert.Equal(t, specialGroupName, dbGroupName)
	assert.Equal(t, specialMemberName, dbMemberName)
}
