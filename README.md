# Azure AD Manager (azentramgr) - Module: Group member extractor

## 1. Application Overview and Objectives

`azentramgr` is a command-line tool for extracting group and member information (first and last name, email, userprincipalname) from an Azure Active Directory (Entra ID) tenant. Its primary objective is to provide an efficient and reliable way to export Azure AD group memberships for analysis, auditing, or reporting purposes.

The tool connects to the Microsoft Graph API using your local Azure CLI credentials (`az login`). It fetches groups and their members, providing powerful server-side filtering options for efficiency.

### Key Features:
- **Comprehensive Export:** Creates both a JSON file and a SQLite database for flexible data analysis.
- **Efficient & Scalable:** Uses concurrency to fetch data quickly and streams results to keep memory usage low, even with large directories.
- **Robust API Usage:** Implements rate limiting to respect Graph API throttling limits and correctly handles pagination to ensure all data is retrieved.
- **Powerful Filtering:** Allows you to target specific groups using exact name matching (including lists) or performant partial matching (`contains`, `startsWith`, `endsWith`).

## 2. Command-Line Arguments

The application's behavior can be customized with the following command-line flags:

| Flag | Type | Default | Description |
|---|---|---|---|
| `-version` | bool | `false` | Print the application version and exit. |
| `-pageSize` | int | `500` | The number of items to retrieve per page for API queries (for both groups and members). Max is 999. |
| `-output-id` | string | `""` (dynamic) | Custom ID for output filenames (e.g., 'my-export'). If empty, a default ID (`<tenant_id>_<timestamp>`) is generated. |
| `-group-name` | string | `""` | Process only groups with exact names. Provide a single name or a comma-separated list. |
| `-group-match` | string | `""` | Process groups using a partial match. Use `*` as a wildcard. E.g., 'Proj*', '*Test*', 'Start*End'. Defaults to 'contains' if no wildcards. Quote argument to avoid shell globbing. |

> **Note:** `-group-name` and `-group-match` are mutually exclusive and cannot be used at the same time.

## 3. Examples on How to Use

### Prerequisites

Microsoft Azure Cli must be installed and available in path. You must be already be authenticated with Azure (if not, the tool will trigger a login attempt). Run the following command and complete the login process before using the extraction tool:
```sh
az login
```

### Basic Usage
To run the extractor with default settings, which will fetch all groups in the tenant:
```sh
./azentramgr
```
This is not recommended for large tenants. Use the filtering flags below for better performance.

### Filtering for Specific Groups (Recommended)

The tool provides two high-performance, server-side filtering methods: `--group-name` for exact matches and `--group-match` for partial matches.

#### Using `--group-name` for Exact Matches

This is the most efficient way to target specific groups.

**Example 1: Extract a single group**
```sh
./azentramgr --group-name "My Production Group"
```

**Example 2: Extract a list of specific groups**
```sh
./azentramgr --group-name "UAT Users,Admins,Project X Members"
```

#### Using `--group-match` for Partial Matches

This flag uses wildcards (`*`) to perform `contains`, `startsWith`, or `endsWith` searches.

> **Important:** You must wrap the match string in quotes (`"`) to prevent your command-line shell from interpreting the `*` as a file glob.

**Example 1: Find groups containing a keyword (default behavior)**
```sh
# Finds group names containing "Legacy"
./azentramgr --group-match "Legacy"
```

**Example 2: Find groups that start with a prefix**
```sh
# Finds group names starting with "PROD-"
./azentramgr --group-match "PROD-*"
```

**Example 3: Find groups that end with a suffix**
```sh
# Finds group names ending with "-Archive"
./azentramgr --group-match "*-Archive"
```

**Example 4: Find groups containing a keyword (explicit)**
```sh
# Finds group names containing "Test"
./azentramgr --group-match "*Test*"
```

**Example 5: Find groups that start and end with specific strings**
```sh
# Finds group names that start with "App-" and end with "-Users"
./azentramgr --group-match "App-*-Users"
```

### Specifying Output Filenames
To provide a custom base name for the output `.json` and `.db` files:
```sh
# This will create "prod_export.json" and "prod_export.db"
./azentramgr -output-id prod_export --group-name "My Production Group"
```
