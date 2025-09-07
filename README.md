# Azure AD Manager (azentramgr) - Module: Group member extractor

## 1. Application Overview and Objectives

`azentramgr` is a command-line tool for extracting group and member information (first and last name, email, userprincipalname) from an Azure Active Directory (Entra ID) tenant. Its primary objective is to provide an efficient and reliable way to export Azure AD group memberships for analysis, auditing, or reporting purposes.

The tool connects to the Microsoft Graph API using your local Azure CLI credentials (`az login`). It fetches all groups, with an option to filter them by name using a regular expression, and then retrieves all members for each of those groups.

### Key Features:
- **Comprehensive Export:** Creates both a JSON file and a SQLite database for flexible data analysis.
- **Efficient & Scalable:** Uses concurrency to fetch data quickly and streams results to keep memory usage low, even with large directories.
- **Robust API Usage:** Implements rate limiting to respect Graph API throttling limits and correctly handles pagination to ensure all data is retrieved.
- **Filtering:** Allows you to target specific groups using regular expressions.

## 2. Command-Line Arguments

The application's behavior can be customized with the following command-line flags:

| Flag | Type | Default | Description |
|---|---|---|---|
| `-version` | bool | `false` | Print the application version and exit. |
| `-pageSize` | int | `500` | The number of items to retrieve per page for API queries (for both groups and members). Max is 999. |
| `-output-id` | string | `""` (dynamic) | Custom ID for output filenames (e.g., 'my-export'). If empty, a default ID (`<tenant_id>_<timestamp>`) is generated. |
| `-group-filter-regex` | string | `""` | Optional regex to filter groups by name. **Note:** Complex patterns can cause performance issues (ReDoS). |

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

### Filtering for Specific Groups
You can use the `-group-filter-regex` flag to be more selective about which groups you extract. The filter uses standard Go regular expressions.

**Example 1: Extract groups with a specific prefix**
```sh
# Extracts all groups whose names start with "PROD_"
./azentramgr -group-filter-regex "^PROD_"
```

**Example 2: Extract groups with a specific suffix**
```sh
# Extracts all groups whose names end with "_DEV"
./azentramgr -group-filter-regex "_DEV$"
```

**Example 3: Extract groups containing a certain keyword**
```sh
# Extracts all groups with "UAT" anywhere in the name
./azentramgr -group-filter-regex "UAT"
```

**Example 4: Extract groups with multiple possible prefixes**
```sh
# Extracts groups starting with either "FINANCE_" or "HR_"
./azentramgr -group-filter-regex "^(FINANCE|HR)_"
```

### Specifying Output Filenames
To provide a custom base name for the output `.json` and `.db` files:
```sh
# This will create "prod_export.json" and "prod_export.db"
./azentramgr -output-id prod_export
```
