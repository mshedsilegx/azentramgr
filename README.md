# Azure AD Manager (azentramgr) - Module: Group member extractor

## 1. Application Overview and Objectives

`azentramgr` is a command-line tool for extracting group and member information (first and last name, email, userprincipalname) from an Azure Active Directory (Entra ID) tenant. Its primary objective is to provide an efficient and reliable way to export Azure AD group memberships for analysis, auditing, or reporting purposes.

The tool supports two authentication methods for connecting to the Microsoft Graph API: interactive login via your local Azure CLI credentials (`az login`) or non-interactive, automated authentication using a service principal (client ID and secret). It fetches groups and their members, providing powerful server-side filtering options for efficiency.

### Key Features:
- **Flexible Authentication:** Supports both interactive `az login` for local use and non-interactive `clientid` with a service principal for automated environments.
- **Comprehensive Export:** Creates both a JSON file and a SQLite database for flexible data analysis.
- **Efficient & Scalable:** Uses concurrency to fetch data quickly and streams results to keep memory usage low, even with large directories.
- **Robust API Usage:** Implements rate limiting to respect Graph API throttling limits and correctly handles pagination to ensure all data is retrieved.
- **Powerful Filtering:** Allows you to target specific groups using exact name matching (including lists) or performant partial matching (`contains`, `startsWith`, `endsWith`).

## 2. Architecture and Design

The application is designed to be a robust and scalable pipeline for extracting data from Azure AD. The architecture is built around a few key principles: concurrency for performance, streaming for low memory usage, and clear separation of concerns.

The core components of the design are:

1.  **Configuration Loading:** The application first loads its configuration from multiple sources, with a clear order of precedence:
    1.  Default values set in the code.
    2.  Values from a JSON file (if specified with `--config`).
    3.  Values from environment variables (for `clientid` authentication).
    4.  Values from command-line flags, which always override any other settings.

2.  **The Extractor Struct:** The `Extractor` is the central struct that holds the application's state, including the configuration, API client, database connection, and a rate limiter. The main logic is executed via its `Run()` method.

3.  **Dispatcher-Worker-Aggregator Model:** The main `Run()` method orchestrates a concurrent pipeline:
    *   **Dispatcher:** A single goroutine is responsible for querying the Graph API to get the list of groups to be processed. It then "dispatches" each group as a task onto a `groupTasks` channel.
    *   **Worker Pool:** A pool of worker goroutines (number configured by `--parallelJobs`) concurrently consumes groups from the `groupTasks` channel. Each worker fetches the members for its assigned group and places the results (JSON data and SQLite records) onto separate result channels.
    *   **Aggregators:** Dedicated goroutines listen on the result channels. One aggregator streams JSON objects to the output file, while others handle batching and inserting records into the SQLite database in transactions.

4.  **Scalability Features:**
    *   **Streaming:** By using channels and processing data as it arrives, the application never holds the entire dataset in memory. This ensures it can handle very large Azure AD tenants without running out of memory.
    *   **API Efficiency:** It uses server-side filtering (`$filter`) to minimize data transfer and client-side processing. It also uses a `rate.Limiter` to respectfully manage the rate of API calls, preventing throttling errors from the Microsoft Graph API.

This concurrent, streaming model allows `azentramgr` to process data efficiently while maintaining a small memory footprint.

## 3. Command-Line Arguments

The application's behavior can be customized with the following command-line flags:

| Flag | Type | Default | Description |
|---|---|---|---|
| `-version` | bool | `false` | Print the application version and exit. |
| `-auth` | string | `"azidentity"` | Authentication method. Can be `'azidentity'` (default, uses `'az login'`) or `'clientid'` (for non-interactive auth). |
| `-config` | string | `""` | Path to a JSON configuration file. Command-line flags override file values. See the "Configuration File" section for details. |
| `-pageSize` | int | `500` | The number of items to retrieve per page for API queries. Max is 999. |
| `-parallelJobs` | int | `16` | Number of concurrent jobs for processing groups. |
| `-output-id` | string | `""` (dynamic) | Custom ID for output filenames (e.g., 'my-export'). If empty, a default ID (`<tenant_id>_<timestamp>`) is generated. |
| `-group-name` | string | `""` | Process only groups with exact names. Provide a single name or a comma-separated list (e.g., `"UAT Users,Admins"`). |
| `-group-match` | string | `""` | Process groups using a partial match. Use `*` as a wildcard. E.g., `'Proj*'`, `'*Test*'`. Quote argument to avoid shell globbing. |

> **Note:** `--group-name` and `--group-match` are mutually exclusive and cannot be used at the same time.

## 4. Examples on How to Use

### Prerequisites

The tool supports two authentication methods. Choose the one that fits your use case.

#### Method 1: Interactive Login (Default)
For interactive use, Microsoft Azure CLI must be installed and available in your path. You must be authenticated with Azure before running the tool.
```sh
az login
```
If your token is expired, the tool may prompt you to run `az login` again.

#### Method 2: Non-Interactive (Service Principal)
For automated execution (e.g., in a CI/CD pipeline), you can use a service principal with a client ID and secret. This method requires the following environment variables to be set:
- `TENANT_ID`: Your Azure tenant ID.
- `CLIENT_ID`: The Application (client) ID of your service principal.
- `CLIENT_SECRET`: The client secret for your service principal.

These values can also be provided in a configuration file (see section 5).

### Basic Usage
To run the extractor with default settings (using `az login` auth), which will fetch all groups in the tenant:
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

### Using Non-Interactive Authentication
To run the tool using a service principal, set the `-auth` flag to `clientid` and ensure your environment variables are configured as described in the prerequisites.
```sh
# Ensure TENANT_ID, CLIENT_ID, and CLIENT_SECRET are set in your environment
./azentramgr --auth clientid --group-name "My Production Group"
```

### Specifying Output Filenames
To provide a custom base name for the output `.json` and `.db` files:
```sh
# This will create "prod_export.json" and "prod_export.db"
./azentramgr --output-id prod_export --group-name "My Production Group"
```

## 5. Configuration File

For more complex or repeated executions, you can use a JSON configuration file to specify all options instead of passing them as command-line flags. Use the `--config` flag to specify the path to your configuration file.

```sh
./azentramgr --config /path/to/my_config.json
```

### Configuration Examples

Below are examples demonstrating how to configure the tool for different scenarios.

**Example 1: Using `groupName` for exact matches**
```json
{
  "pageSize": 500,
  "parallelJobs": 16,
  "outputId": "finance-export",
  "groupName": "Finance Users,Marketing Leads"
}
```

**Example 2: Using `groupMatch` for a partial match**
```json
{
  "pageSize": 500,
  "parallelJobs": 16,
  "outputId": "test-groups-export",
  "groupMatch": "*-Test-*"
}
```

**Example 3: Using `clientid` for non-interactive authentication**
```json
{
  "auth": "clientid",
  "tenantId": "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx",
  "clientId": "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx",
  "clientSecret": "your-client-secret-value-here",
  "outputId": "automated-export",
  "groupMatch": "PROD-*"
}
```

### Complete Structure Reference
The table below lists all possible attributes that can be set in the `config.json` file.

| JSON Key | Type | Description |
|---|---|---|
| `auth` | string | The authentication method. Can be `'azidentity'` (default) or `'clientid'`. |
| `tenantId` | string | Your Azure tenant ID. Required when `auth` is `'clientid'`. |
| `clientId` | string | The Application (client) ID. Required when `auth` is `'clientid'`. |
| `clientSecret` | string | The client secret for the service principal. Required when `auth` is `'clientid'`. |
| `pageSize` | integer | The number of items to retrieve per page for API queries. Max is 999. |
| `parallelJobs` | integer | The number of concurrent jobs for processing groups. Default is 16. |
| `outputId` | string | Custom base name for the output `.json` and `.db` files (e.g., "my-export"). |
| `groupName` | string | A single group name or a comma-separated list of exact group names to process. |
| `groupMatch` | string | A partial match string for group names, using `*` as a wildcard. |

**Note:** `groupName` and `groupMatch` are mutually exclusive and should not be set at the same time in the configuration.

### Precedence
The final configuration is determined by the following order of precedence (where items lower in the list override those above them):
1. Default values in the code.
2. Values from the `config.json` file.
3. Values from environment variables (`TENANT_ID`, `CLIENT_ID`, `CLIENT_SECRET`).
4. Values from command-line flags.

For example, if your `config.json` specifies `parallelJobs: 16`, running the following command will execute with 32 jobs:
```sh
./azentramgr --config /path/to/my_config.json --parallelJobs 32
```
