package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"

	_ "github.com/glebarez/sqlite"
)

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
