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
	// 1. Set up Azure CLI credentials.
	// This credential type uses the user's Azure CLI login session.
	// Run `az login` to authenticate with the Azure CLI.
	cred, err := azidentity.NewAzureCLICredential(nil)
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