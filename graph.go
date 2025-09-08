package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/golang-jwt/jwt/v5"
)

// JSONMember represents the detailed structure for a user member in the JSON output.
type JSONMember struct {
	GivenName         string `json:"givenName,omitempty"`
	Mail              string `json:"mail,omitempty"`
	Surname           string `json:"surname,omitempty"`
	UserPrincipalName string `json:"userPrincipalName"`
}

// JSONGroup represents the structure for each group in the final JSON output.
type JSONGroup struct {
	ADGroupName       string       `json:"ADGroupName"`
	ADGroupMemberName []JSONMember `json:"ADGroupMemberName,omitempty"` // Use omitempty to hide if nil/empty
}

func int32Ptr(i int) *int32 {
	v := int32(i)
	return &v
}

func strPtr(s string) *string {
	return &s
}

func getTenantID(ctx context.Context, cred *azidentity.AzureCLICredential) (string, error) {
	token, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{"https://graph.microsoft.com/.default"}})
	if err != nil {
		return "", fmt.Errorf("failed to get token: %w", err)
	}
	parser := new(jwt.Parser)
	claims := jwt.MapClaims{}
	// Note: We use ParseUnverified because we don't need to validate the token's signature.
	// We are only extracting the tenant ID claim ("tid") from a token that we have just
	// received directly from Azure AD, which we trust as the source.
	// This is NOT safe for authenticating incoming requests.
	_, _, err = parser.ParseUnverified(token.Token, claims)
	if err != nil {
		return "", fmt.Errorf("failed to parse token: %w", err)
	}
	tid, ok := claims["tid"].(string)
	if !ok {
		return "", errors.New("could not find 'tid' claim in token")
	}
	return tid, nil
}
