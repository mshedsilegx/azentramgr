package main

import (
	"context"
	"encoding/json"
	"flag"
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

	// 2. Setup a temporary file-based database
	dbFile, err := os.CreateTemp("", "test-aggregator-*.db")
	require.NoError(t, err)
	dbPath := dbFile.Name()
	dbFile.Close()
	defer os.Remove(dbPath)

	db, err := setupDatabase(context.Background(), dbPath)
	require.NoError(t, err)
	defer db.Close()

	// 3. Setup temp file for JSON output
	tmpFile, err := os.CreateTemp("", "test-output-*.json")
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
	go streamJsonToFile(&aggregatorsWg, jsonResults, extractor.config.JsonOutputFile)
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
	jsonContent, err := os.ReadFile(tmpFile.Name())
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

func TestLoadConfig(t *testing.T) {
	// Helper function to reset flags after each test case
	resetFlags := func() {
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	}

	// --- Test Case 1: Default auth method ---
	t.Run("defaults to azidentity", func(t *testing.T) {
		resetFlags()
		os.Args = []string{"cmd"} // No flags
		config, err := LoadConfig()
		require.NoError(t, err)
		assert.Equal(t, "azidentity", config.AuthMethod)
	})

	// --- Test Case 2: clientid auth with environment variables ---
	t.Run("clientid auth with env vars", func(t *testing.T) {
		resetFlags()
		os.Args = []string{"cmd", "-auth", "clientid"}
		t.Setenv("TENANT_ID", "env_tenant_id")
		t.Setenv("CLIENT_ID", "env_client_id")
		t.Setenv("CLIENT_SECRET", "env_client_secret")

		config, err := LoadConfig()
		require.NoError(t, err)
		assert.Equal(t, "clientid", config.AuthMethod)
		assert.Equal(t, "env_tenant_id", config.TenantID)
		assert.Equal(t, "env_client_id", config.ClientID)
		assert.Equal(t, "env_client_secret", config.ClientSecret)
	})

	// --- Test Case 3: clientid auth with config file ---
	t.Run("clientid auth with config file", func(t *testing.T) {
		resetFlags()
		// Create a temporary config file
		configFile, err := os.CreateTemp("", "config-*.json")
		require.NoError(t, err)
		defer os.Remove(configFile.Name())

		configData := map[string]string{
			"auth":         "clientid",
			"tenantId":     "file_tenant_id",
			"clientId":     "file_client_id",
			"clientSecret": "file_client_secret",
		}
		encoder := json.NewEncoder(configFile)
		err = encoder.Encode(configData)
		require.NoError(t, err)
		configFile.Close()

		os.Args = []string{"cmd", "-config", configFile.Name()}
		config, err := LoadConfig()
		require.NoError(t, err)
		assert.Equal(t, "clientid", config.AuthMethod)
		assert.Equal(t, "file_tenant_id", config.TenantID)
		assert.Equal(t, "file_client_id", config.ClientID)
		assert.Equal(t, "file_client_secret", config.ClientSecret)
	})

	// --- Test Case 4: clientid auth missing credentials ---
	t.Run("clientid auth missing credentials", func(t *testing.T) {
		resetFlags()
		os.Args = []string{"cmd", "-auth", "clientid"}
		// Ensure env vars are not set
		t.Setenv("TENANT_ID", "")
		t.Setenv("CLIENT_ID", "")
		t.Setenv("CLIENT_SECRET", "")

		_, err := LoadConfig()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "TENANT_ID must be set")
	})

	// --- Test Case 5: Invalid auth method ---
	t.Run("invalid auth method", func(t *testing.T) {
		resetFlags()
		os.Args = []string{"cmd", "-auth", "invalidauth"}
		_, err := LoadConfig()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid auth method")
	})

	// --- Test Case 6: Flag precedence over config file and env vars ---
	t.Run("flags override config file and env vars", func(t *testing.T) {
		resetFlags()
		// Set env vars
		t.Setenv("TENANT_ID", "env_tenant")
		t.Setenv("CLIENT_ID", "env_client")

		// Create a temporary config file
		configFile, err := os.CreateTemp("", "config-*.json")
		require.NoError(t, err)
		defer os.Remove(configFile.Name())
		configData := map[string]interface{}{
			"auth":     "clientid",
			"tenantId": "file_tenant",
			"clientId": "file_client",
			"pageSize": 100,
			"outputId": "file_id",
		}
		encoder := json.NewEncoder(configFile)
		err = encoder.Encode(configData)
		require.NoError(t, err)
		configFile.Close()

		os.Args = []string{
			"cmd",
			"-config", configFile.Name(),
			"-auth", "azidentity", // This should override the file's "clientid"
			"-pageSize", "200", // This should override the file's 100
		}
		config, err := LoadConfig()
		require.NoError(t, err)
		assert.Equal(t, "azidentity", config.AuthMethod)
		assert.Equal(t, 200, config.PageSize)
		assert.Equal(t, "file_id", config.OutputID)    // This should be from the file
		assert.Equal(t, "env_tenant", config.TenantID) // This should be from the env var
	})

	// --- Test Case 7: Incompatible flags with --use-cache ---
	t.Run("incompatible flags with use-cache", func(t *testing.T) {
		// Create a dummy file to pass the existence check
		dummyFile, err := os.CreateTemp("", "dummy-cache-*.db")
		require.NoError(t, err)
		defer os.Remove(dummyFile.Name())
		dummyFile.Close()

		testCases := []struct {
			flag   string
			errMsg string
		}{
			{"--auth", "--auth is incompatible with --use-cache"},
			{"--pageSize", "--pageSize is incompatible with --use-cache"},
			{"--parallelJobs", "--parallelJobs is incompatible with --use-cache"},
		}

		for _, tc := range testCases {
			t.Run(tc.flag, func(t *testing.T) {
				resetFlags()
				args := []string{"cmd", "--use-cache", dummyFile.Name()}
				// Provide a valid value for each flag type
				switch tc.flag {
				case "--pageSize", "--parallelJobs":
					args = append(args, tc.flag, "1")
				default:
					args = append(args, tc.flag, "some-value")
				}
				os.Args = args
				_, err := LoadConfig()
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errMsg)
			})
		}
	})
}

func TestRunFromCache(t *testing.T) {
	// --- 1. Setup a temporary cache database ---
	ctx := context.Background()
	dbFile, err := os.CreateTemp("", "test-cache-*.db")
	require.NoError(t, err)
	dbPath := dbFile.Name()
	dbFile.Close() // Close the file so the database driver can open it
	defer os.Remove(dbPath)

	db, err := setupDatabase(ctx, dbPath)
	require.NoError(t, err)

	// --- 2. Populate the database with test data ---
	users := []SQLiteUser{
		{UserPrincipalName: "user1@test.com", GivenName: "User", Surname: "One", Mail: "user1@test.com"},
		{UserPrincipalName: "user2@test.com", GivenName: "User", Surname: "Two", Mail: "user2@test.com"},
		{UserPrincipalName: "user3@test.com", GivenName: "User", Surname: "Three", Mail: "user3@test.com"},
	}
	groups := []SQLiteGroupMember{
		{GroupName: "Project-Alpha-Test", MemberName: "user1@test.com"},
		{GroupName: "Project-Alpha-Test", MemberName: "user2@test.com"},
		{GroupName: "Project-Beta-Prod", MemberName: "user3@test.com"},
		{GroupName: "Finance-Users", MemberName: "user1@test.com"},
	}

	tx, err := db.Begin()
	require.NoError(t, err)
	userStmt, err := tx.Prepare("INSERT INTO entraUsers (userPrincipalName, givenName, mail, surname) VALUES (?, ?, ?, ?)")
	require.NoError(t, err)
	defer userStmt.Close()
	for _, u := range users {
		_, err := userStmt.Exec(u.UserPrincipalName, u.GivenName, u.Mail, u.Surname)
		require.NoError(t, err)
	}
	groupStmt, err := tx.Prepare("INSERT INTO entraGroups (groupName, groupMember) VALUES (?, ?)")
	require.NoError(t, err)
	defer groupStmt.Close()
	for _, g := range groups {
		_, err := groupStmt.Exec(g.GroupName, g.MemberName)
		require.NoError(t, err)
	}
	err = tx.Commit()
	require.NoError(t, err)
	db.Close() // Close the db so the function can reopen it

	// --- 3. Define and run test cases ---
	testCases := []struct {
		name               string
		config             Config
		expectedGroupCount int
		expectedTotalUsers int
		expectedGroups     map[string][]string // map[groupName] -> []userPrincipalNames
	}{
		{
			name: "no filter",
			config: Config{
				UseCache: dbPath,
				OutputID: "test-no-filter",
			},
			expectedGroupCount: 3,
			expectedTotalUsers: 4,
			expectedGroups: map[string][]string{
				"Project-Alpha-Test": {"user1@test.com", "user2@test.com"},
				"Project-Beta-Prod":  {"user3@test.com"},
				"Finance-Users":      {"user1@test.com"},
			},
		},
		{
			name: "group-name exact match",
			config: Config{
				UseCache:  dbPath,
				OutputID:  "test-group-name",
				GroupName: "Finance-Users,Project-Beta-Prod",
			},
			expectedGroupCount: 2,
			expectedTotalUsers: 2,
			expectedGroups: map[string][]string{
				"Project-Beta-Prod": {"user3@test.com"},
				"Finance-Users":     {"user1@test.com"},
			},
		},
		{
			name: "group-match contains",
			config: Config{
				UseCache:   dbPath,
				OutputID:   "test-group-match-contains",
				GroupMatch: "Project",
			},
			expectedGroupCount: 2,
			expectedTotalUsers: 3,
			expectedGroups: map[string][]string{
				"Project-Alpha-Test": {"user1@test.com", "user2@test.com"},
				"Project-Beta-Prod":  {"user3@test.com"},
			},
		},
		{
			name: "group-match startsWith",
			config: Config{
				UseCache:   dbPath,
				OutputID:   "test-group-match-starts",
				GroupMatch: "Project-Alpha*",
			},
			expectedGroupCount: 1,
			expectedTotalUsers: 2,
			expectedGroups: map[string][]string{
				"Project-Alpha-Test": {"user1@test.com", "user2@test.com"},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// --- 4. Run the function under test ---
			err := runFromCache(tc.config)
			require.NoError(t, err)

			// --- 5. Verify the JSON output ---
			jsonFile := tc.config.OutputID + ".json"
			defer os.Remove(jsonFile)

			content, err := os.ReadFile(jsonFile)
			require.NoError(t, err)

			var results []JSONGroup
			err = json.Unmarshal(content, &results)
			require.NoError(t, err)

			assert.Len(t, results, tc.expectedGroupCount)

			totalUsers := 0
			resultGroups := make(map[string][]string)
			for _, group := range results {
				totalUsers += len(group.ADGroupMemberName)
				var members []string
				for _, member := range group.ADGroupMemberName {
					members = append(members, member.UserPrincipalName)
				}
				resultGroups[group.ADGroupName] = members
			}

			assert.Equal(t, tc.expectedTotalUsers, totalUsers)
			assert.Equal(t, tc.expectedGroups, resultGroups)
		})
	}
}
