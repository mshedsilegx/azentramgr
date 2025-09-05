package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	"github.com/microsoftgraph/msgraph-sdk-go/groups"
	"github.com/microsoftgraph/msgraph-sdk-go/models"
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

	// 3. Make the initial API call to list groups.
	result, err := getGroupsWithLoginRetry(context.Background(), client)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	// 4. Iterate over the results and print display names
	responseGroups := result.GetValue()
	for _, group := range responseGroups {
		if group.GetDisplayName() != nil {
			fmt.Printf("%s\n", *group.GetDisplayName())
		}
	}
}

func getGroupsWithLoginRetry(ctx context.Context, client *msgraphsdk.GraphServiceClient) (models.GroupCollectionResponseable, error) {
	requestParameters := &groups.GroupsRequestBuilderGetQueryParameters{
		Select: []string{"displayName"},
	}
	options := &groups.GroupsRequestBuilderGetRequestConfiguration{
		QueryParameters: requestParameters,
	}

	result, err := client.Groups().Get(ctx, options)
	if err != nil {
		var authFailedErr *azidentity.AuthenticationFailedError
		if errors.As(err, &authFailedErr) {
			fmt.Fprintln(os.Stderr, "Warning: Authentication failed. This may be because you are not logged into the Azure CLI.")
			fmt.Fprintln(os.Stderr, "Attempting to run 'az login' to re-authenticate...")

			cmd := exec.Command("az", "login")
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr

			if runErr := cmd.Run(); runErr != nil {
				return nil, fmt.Errorf("failed to run 'az login' interactively. Is the Azure CLI installed and in your PATH? Error: %w", runErr)
			}

			fmt.Fprintln(os.Stderr, "Login successful. Retrying API call...")
			return client.Groups().Get(ctx, options)
		}
		return nil, fmt.Errorf("getting groups failed: %w", err)
	}
	return result, nil
}