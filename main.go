package main

import (
	"context"
	"fmt"
	"os"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	groups "github.com/microsoftgraph/msgraph-sdk-go/groups"
)

func main() {
	// 1. Set up Interactive (Browser) Credentials
	// Replace with your Entra ID tenant ID and the Client ID of your registered application.
	// You can find these in the Azure portal under your app registration's "Overview" blade.
	tenantID := os.Getenv("AZURE_TENANT_ID")
	clientID := os.Getenv("AZURE_CLIENT_ID")

	cred, err := azidentity.NewInteractiveBrowserCredential(
		&azidentity.InteractiveBrowserCredentialOptions{
			ClientID: clientID,
			TenantID: tenantID,
		},
	)
	if err != nil {
		fmt.Printf("Error creating credential: %v\n", err)
		os.Exit(1)
	}

	// 2. Create the Microsoft Graph client
	// This is the new, correct way to create the client in recent SDK versions.
	client, err := msgraphsdk.NewGraphServiceClientWithCredentials(cred, nil)
	if err != nil {
		fmt.Printf("Error creating Graph client: %v\n", err)
		os.Exit(1)
	}

	// 3. Make the initial API call to list groups and select only the displayName.
	requestParameters := &groups.GroupsRequestBuilderGetQueryParameters{
		Select: []string{"displayName"},
	}
	options := &groups.GroupsRequestBuilderGetRequestConfiguration{
		QueryParameters: requestParameters,
	}

	result, err := client.Groups().Get(context.Background(), options)
	if err != nil {
		fmt.Printf("Error getting groups: %v\n", err)
		os.Exit(1)
	}

	// 4. Iterate over the results and print display names
	groups := result.GetValue()
	for _, group := range groups {
		if group.GetDisplayName() != nil {
			fmt.Printf("%s\n", *group.GetDisplayName())
		}
	}
}