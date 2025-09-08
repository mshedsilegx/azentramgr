package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	_ "github.com/glebarez/sqlite"
)

func runFromCache(config Config) error {
	// --- 1. Print informational messages ---
	fileInfo, err := os.Stat(config.UseCache)
	if err != nil {
		// This should have been caught by validation in LoadConfig, but check again.
		return fmt.Errorf("could not stat cache file: %w", err)
	}
	log.Printf("Remark: Querying from cache file %s (last modified: %s). Data may be outdated.", config.UseCache, fileInfo.ModTime().Format(time.RFC1123))
	log.Println("Remark: A new SQLite file will not be created when running from cache.")
	if config.AuthMethod != "azidentity" || config.PageSize != 500 || config.ParallelJobs != 16 {
		log.Println("Remark: Flags like --auth, --pageSize, and --parallelJobs are ignored when using --use-cache.")
	}

	// --- 2. Connect to the cache DB ---
	db, err := sql.Open("sqlite", config.UseCache)
	if err != nil {
		return fmt.Errorf("failed to open cache database: %w", err)
	}
	defer db.Close()

	// --- 3. Build the SQL query ---
	query := `
		SELECT g.groupName, u.userPrincipalName, u.givenName, u.mail, u.surname
		FROM entraGroups g
		JOIN entraUsers u ON g.groupMember = u.userPrincipalName
	`
	var args []interface{}
	var whereClauses []string

	if config.GroupName != "" {
		names := strings.Split(config.GroupName, ",")
		placeholders := strings.Repeat("?,", len(names)-1) + "?"
		whereClauses = append(whereClauses, "g.groupName IN ("+placeholders+")")
		for _, name := range names {
			args = append(args, strings.TrimSpace(name))
		}
	} else if config.GroupMatch != "" {
		matchStr := strings.TrimSpace(config.GroupMatch)
		// Translate Graph wildcard to SQL LIKE wildcard
		sqlPattern := strings.ReplaceAll(matchStr, "*", "%")
		if !strings.HasPrefix(sqlPattern, "%") && !strings.HasSuffix(sqlPattern, "%") {
			// If no wildcards were in the original string, default to a 'contains' search
			sqlPattern = "%" + sqlPattern + "%"
		}
		whereClauses = append(whereClauses, "g.groupName LIKE ?")
		args = append(args, sqlPattern)
	}

	if len(whereClauses) > 0 {
		query += " WHERE " + strings.Join(whereClauses, " AND ")
	}
	query += " ORDER BY g.groupName, u.userPrincipalName;"

	log.Printf("Executing SQL query against cache...")

	// --- 4. Execute the query ---
	rows, err := db.Query(query, args...)
	if err != nil {
		return fmt.Errorf("failed to execute query on cache: %w", err)
	}
	defer rows.Close()

	// --- 5. Process and stream results ---
	jsonResults := make(chan JSONGroup, 100)
	var wg sync.WaitGroup
	wg.Add(1)

	// Determine base filename for JSON output
	var baseName string
	if config.OutputID != "" {
		baseName = config.OutputID
	} else {
		baseName = fmt.Sprintf("cached-query_%s", time.Now().Format("20060102-150405"))
	}
	jsonOutputFile := baseName + ".json"

	go streamJsonToFile(&wg, jsonResults, jsonOutputFile)

	var currentGroup *JSONGroup
	var groupCount int
	for rows.Next() {
		var groupName, upn, givenName, mail, surname sql.NullString
		if err := rows.Scan(&groupName, &upn, &givenName, &mail, &surname); err != nil {
			return fmt.Errorf("failed to scan row from cache: %w", err)
		}

		// Since the query is ordered by group name, we can process one group at a time.
		if currentGroup == nil || currentGroup.ADGroupName != groupName.String {
			// If this isn't the very first group, send the completed one.
			if currentGroup != nil {
				jsonResults <- *currentGroup
			}
			// Start a new group
			currentGroup = &JSONGroup{ADGroupName: groupName.String, ADGroupMemberName: []JSONMember{}}
			groupCount++
		}

		member := JSONMember{
			UserPrincipalName: upn.String,
			GivenName:         givenName.String,
			Mail:              mail.String,
			Surname:           surname.String,
		}
		currentGroup.ADGroupMemberName = append(currentGroup.ADGroupMemberName, member)
	}

	// Send the last processed group
	if currentGroup != nil {
		jsonResults <- *currentGroup
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("error during row iteration: %w", err)
	}

	log.Printf("Found %d matching groups in cache.", groupCount)

	close(jsonResults)
	wg.Wait()

	return nil
}
