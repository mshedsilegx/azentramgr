package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	msgraphgocore "github.com/microsoftgraph/msgraph-sdk-go-core"
	"github.com/microsoftgraph/msgraph-sdk-go/groups"
	"github.com/microsoftgraph/msgraph-sdk-go/models"
)

func main() {
	pageSize := flag.Int("pageSize", 750, "The number of groups to retrieve per page. Max is 999.")
	flag.Parse()

	if *pageSize > 999 || *pageSize < 1 {
		fmt.Fprintln(os.Stderr, "Error: pageSize must be between 1 and 999.")
		os.Exit(1)
	}

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
	initialResult, err := getGroupsWithLoginRetry(context.Background(), client, *pageSize)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	// The PageIterator needs a concrete type, so we have to type assert the result.
	result, ok := initialResult.(*models.GroupCollectionResponse)
	if !ok {
		fmt.Fprintln(os.Stderr, "Error: could not perform type assertion on the Graph API result.")
		os.Exit(1)
	}

	// 4. Create a Page Iterator to process all pages of results
	pageIterator, err := msgraphgocore.NewPageIterator[*models.Group](result, client.GetAdapter(), models.CreateGroupCollectionResponseFromDiscriminatorValue)
	if err != nil {
		fmt.Printf("Error creating page iterator: %v\n", err)
		os.Exit(1)
	}

	// 5. Iterate over all pages and process each group
	err = pageIterator.Iterate(context.Background(), func(group *models.Group) bool {
		if group.GetDisplayName() != nil {
			fmt.Printf("%s\n", *group.GetDisplayName())
		}
		return true // Continue iterating
	})

	if err != nil {
		fmt.Printf("Error during iteration: %v\n", err)
		os.Exit(1)
	}
}

func getGroupsWithLoginRetry(ctx context.Context, client *msgraphsdk.GraphServiceClient, pageSize int) (models.GroupCollectionResponseable, error) {
	requestParameters := &groups.GroupsRequestBuilderGetQueryParameters{
		Select:  []string{"displayName"},
		Orderby: []string{"displayName asc"},
		Top:     int32Ptr(pageSize),
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

// int32Ptr returns a pointer to an int32 value.
func int32Ptr(i int) *int32 {
	v := int32(i)
	return &v
}