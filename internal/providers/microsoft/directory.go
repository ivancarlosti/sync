package microsoft

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/ivancarlosti/sync/internal/providers"
)

// DirectoryReader feeds the selects of the Admin operations forms: the users and
// groups an operator can name without typing an address. Both listings are the
// plain Graph collections, paged through `@odata.nextLink`; the fields are the
// same ones the directory report reads, so the two screens cannot disagree about
// a display name or a guest flag.

// ListDirectoryUsers implements providers.DirectoryReader.
func (p *Provider) ListDirectoryUsers(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens) ([]providers.DirectoryUser, error) {
	if err := requireToken(tokens); err != nil {
		return nil, err
	}
	target := collectionTarget("/users", []string{
		"id", "displayName", "userPrincipalName", "mail", "userType",
	}, url.Values{"$top": {strconv.Itoa(reportPageSize)}})
	users := make([]providers.DirectoryUser, 0, 256)
	for page := 0; page < reportPageLimit; page++ {
		var answer struct {
			Value []struct {
				DisplayName       string `json:"displayName"`
				UserPrincipalName string `json:"userPrincipalName"`
				Mail              string `json:"mail"`
				UserType          string `json:"userType"`
			} `json:"value"`
			NextLink string `json:"@odata.nextLink"`
		}
		if err := p.getJSON(ctx, target, tokens.AccessToken, &answer); err != nil {
			return nil, err
		}
		for _, user := range answer.Value {
			email := strings.TrimSpace(user.UserPrincipalName)
			if email == "" {
				email = strings.TrimSpace(user.Mail)
			}
			if email == "" {
				continue
			}
			users = append(users, providers.DirectoryUser{
				Email: email,
				Name:  strings.TrimSpace(user.DisplayName),
				Guest: strings.EqualFold(strings.TrimSpace(user.UserType), "guest") ||
					strings.Contains(strings.ToLower(user.UserPrincipalName), "#ext#"),
			})
		}
		if answer.NextLink == "" {
			return users, nil
		}
		if !strings.HasPrefix(answer.NextLink, baseURL+"/") {
			return nil, fmt.Errorf("microsoft graph: a directory page link left Graph (%s)", safeURL(answer.NextLink))
		}
		target = answer.NextLink
	}
	return users, nil
}

// ListDirectoryGroups implements providers.DirectoryReader.
func (p *Provider) ListDirectoryGroups(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens) ([]providers.DirectoryGroup, error) {
	if err := requireToken(tokens); err != nil {
		return nil, err
	}
	target := collectionTarget("/groups", []string{
		"id", "displayName", "mail", "mailEnabled", "securityEnabled", "groupTypes",
	}, url.Values{"$top": {strconv.Itoa(reportPageSize)}})
	groups := make([]providers.DirectoryGroup, 0, 128)
	for page := 0; page < reportPageLimit; page++ {
		var answer struct {
			Value []struct {
				DisplayName string `json:"displayName"`
				Mail        string `json:"mail"`
			} `json:"value"`
			NextLink string `json:"@odata.nextLink"`
		}
		if err := p.getJSON(ctx, target, tokens.AccessToken, &answer); err != nil {
			return nil, err
		}
		for _, group := range answer.Value {
			email := strings.TrimSpace(group.Mail)
			if email == "" {
				continue
			}
			groups = append(groups, providers.DirectoryGroup{
				Email: email,
				Name:  strings.TrimSpace(group.DisplayName),
			})
		}
		if answer.NextLink == "" {
			return groups, nil
		}
		if !strings.HasPrefix(answer.NextLink, baseURL+"/") {
			return nil, fmt.Errorf("microsoft graph: a directory page link left Graph (%s)", safeURL(answer.NextLink))
		}
		target = answer.NextLink
	}
	return groups, nil
}

// Compile time guard: a renamed method must fail the build.
var _ providers.DirectoryReader = (*Provider)(nil)
