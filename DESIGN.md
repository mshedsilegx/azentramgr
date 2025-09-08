# AZ Entra Extractor - Design and Specifications

## 1. PowerShell Logic Implementation

Implement the logic below from PowerShell, and convert to Golang:

```powershell
Write-Host "Extraction of azureadgroupmembers is started"
    Try{
        $adgrouplist = @()
        $adgrouplist = az ad group list --query [*].[displayName] --output tsv
        $adgrouplistcount = $adgrouplist.Count
        Write-Output "[" | out-file -Append -encoding unicode -filepath "adgroupmembers.json"
        foreach($adgrp in $adgrouplist)
        {
	        Write-Host "Data extraction for group $adgrp"
            Write-Output "{" | out-file -Append -encoding unicode -filepath "adgroupmembers.json"
            Write-Output """ADGroupName"":""$adgrp""" | out-file -Append -encoding unicode -filepath "adgroupmembers.json"
	
	        $cnt8=az ad group member list --group $adgrp --output tsv
	        if ($cnt8.count -ne 0)
	        {
	        Write-Output ",""ADGroupMemberName"":" | out-file -Append -encoding unicode -filepath "adgroupmembers.json"
            az ad group member list --group $adgrp --output json | out-file -Append -encoding unicode -filepath "adgroupmembers.json"
	        }
            $adgrouplistcount=$adgrouplistcount-1
            If($adgrouplistcount -ge 1){
                Write-Output "}," | out-file -Append -encoding unicode -filepath "adgroupmembers.json"
                }
                Else{
                Write-Output "}" | out-file -Append -encoding unicode -filepath "adgroupmembers.json"
                }
        }
        Write-Output "]" | out-file -Append -encoding unicode -filepath "adgroupmembers.json"
    }
    catch{
      Write-Output "Issue with GroupMembers Listing" | out-file -encoding unicode -filepath "adgroupmembers.json"
    }
```

User record example:

```json
{
    "givenName": "XXXX",
    "mail": "XXXX",
    "surname": "XXXX",
    "userPrincipalName": "XXXX"
}
```

## 2. Application Features

### 2.1 Output File Configuration

Make the output JSON file location and name an argument. It defaults as: `adgroupmembers.json` in the current folder.

### 2.2 SQLite Database Generation

Produce also a SQLite (use pure golang driver, no C dependencies) file called `<entra_tenant_id>_YYYYMMDD-hhmmss.db` with the following table names:

#### `entraGroups` Table

```
-------------------+-------------
     groupName     | groupMember
-------------------+-------------
```

And include indexes to optimize query:

```sql
SELECT groupName FROM entraGroups WHERE groupMember LIKE "<name_of_member>%"
```

And:

```sql
SELECT groupMember FROM entraGroups WHERE groupName LIKE "<name_of_group>%"
```

`groupMember` is the `UserPrincipalName`.

#### `entraUsers` Table

```
-------------------+-----------+------+---------
 UserPrincipalName | givenName | mail | surname
-------------------+-----------+------+---------
```

With an index on `UserPrincipalName`.

### 2.3 Parallel JSON and SQLite Generation

Generate both JSON file (with a streaming encoder, with explicit write buffering) and SQLite DB in parallel, to minimize memory usage. For SQLite inserts, implement a large transaction commit for bulk inserts, disable synchronous writes, and use prepared statements.

### 2.4 Group Filtering Option

Implement an option to filter groups via `regexp`, instead of considering them all.

### 2.5 Progress Tracking

Add total number of groups to process, and for each group a percentage of completion.

### 2.6 Handling Non-Alphanumeric Characters

Group name and member can contain non-alphabet/digit characters, make sure that will not cause escaping issues.

## 3. Rules and Guardrails

See file: `RULES.md`