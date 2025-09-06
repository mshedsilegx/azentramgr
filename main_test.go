package main

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAggregatorFunctions(t *testing.T) {
	// 1. Define test data
	specialGroupName := `Group Name with "Quotes" & 'Apostrophes' \/`
	testMember := JSONMember{
		GivenName:         "Testy",
		Surname:           `Mc'Testerson`,
		Mail:              "testy.mctesterson@example.com",
		UserPrincipalName: "testy.mctesterson_example.com#EXT#@yourtenant.onmicrosoft.com",
	}

	// 2. Setup in-memory database and all tables
	// Call setupDatabase and use the db connection it returns
	db, err := setupDatabase(context.Background(), ":memory:")
	require.NoError(t, err)
	defer db.Close()

	// 3. Setup temp file for JSON output
	tmpFile, err := ioutil.TempFile("", "test-output-*.json")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	// 4. Create a minimal Extractor
	config := Config{JsonOutputFile: tmpFile.Name()}
	extractor := &Extractor{
		config: config,
		ctx:    context.Background(),
		db:     db,
	}

	// 5. Run the aggregator functions with test data
	jsonResults := make(chan JSONGroup, 1)
	sqliteGroupResults := make(chan SQLiteGroupMember, 1)
	sqliteUserResults := make(chan SQLiteUser, 1)
	var aggregatorsWg sync.WaitGroup

	aggregatorsWg.Add(3)
	go extractor.streamJsonToFile(&aggregatorsWg, jsonResults)
	go extractor.processSQLiteInserts(&aggregatorsWg, sqliteGroupResults)
	go extractor.processUserInserts(&aggregatorsWg, sqliteUserResults)

	// Send test data
	jsonResults <- JSONGroup{
		ADGroupName:       specialGroupName,
		ADGroupMemberName: []JSONMember{testMember},
	}
	sqliteGroupResults <- SQLiteGroupMember{
		GroupName:  specialGroupName,
		MemberName: testMember.UserPrincipalName,
	}
	sqliteUserResults <- SQLiteUser{
		UserPrincipalName: testMember.UserPrincipalName,
		GivenName:         testMember.GivenName,
		Mail:              testMember.Mail,
		Surname:           testMember.Surname,
	}
	close(jsonResults)
	close(sqliteGroupResults)
	close(sqliteUserResults)

	aggregatorsWg.Wait()

	// 6. Verify the output
	// Verify JSON file content
	jsonContent, err := ioutil.ReadFile(tmpFile.Name())
	require.NoError(t, err)
	var outputGroups []JSONGroup
	err = json.Unmarshal(jsonContent, &outputGroups)
	require.NoError(t, err, "Failed to unmarshal JSON content: %s", string(jsonContent))
	require.Len(t, outputGroups, 1)
	assert.Equal(t, specialGroupName, outputGroups[0].ADGroupName)
	require.Len(t, outputGroups[0].ADGroupMemberName, 1)
	assert.Equal(t, testMember, outputGroups[0].ADGroupMemberName[0])

	// Verify entraGroups table content
	var dbGroupName, dbMemberName string
	err = db.QueryRow("SELECT groupName, groupMember FROM entraGroups WHERE groupName = ?", specialGroupName).Scan(&dbGroupName, &dbMemberName)
	require.NoError(t, err)
	assert.Equal(t, specialGroupName, dbGroupName)
	assert.Equal(t, testMember.UserPrincipalName, dbMemberName)

	// Verify entraUsers table content
	var dbUser SQLiteUser
	err = db.QueryRow("SELECT UserPrincipalName, givenName, mail, surname FROM entraUsers WHERE UserPrincipalName = ?", testMember.UserPrincipalName).Scan(&dbUser.UserPrincipalName, &dbUser.GivenName, &dbUser.Mail, &dbUser.Surname)
	require.NoError(t, err)
	assert.Equal(t, testMember.UserPrincipalName, dbUser.UserPrincipalName)
	assert.Equal(t, testMember.GivenName, dbUser.GivenName)
	assert.Equal(t, testMember.Mail, dbUser.Mail)
	assert.Equal(t, testMember.Surname, dbUser.Surname)
}
