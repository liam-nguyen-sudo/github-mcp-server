package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/github/github-mcp-server/pkg/translations"
	"github.com/google/go-github/v69/github"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/viper"
)

// graphQLRequest represents a GitHub GraphQL API request
type graphQLRequest struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables,omitempty"`
}

// executeGraphQL executes a GraphQL query against the GitHub API
func executeGraphQL(ctx context.Context, client *github.Client, query string, variables map[string]interface{}, result interface{}) error {
	requestBody := graphQLRequest{
		Query:     query,
		Variables: variables,
	}

	payload, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("failed to marshal GraphQL request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.github.com/graphql", bytes.NewBuffer(payload))
	if err != nil {
		return fmt.Errorf("failed to create GraphQL request: %w", err)
	}

	// Set required headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	token := viper.GetString("personal_access_token")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

	// Copy authorization header from the client's transport
	if transport := client.Client().Transport; transport != nil {
		dummy, _ := http.NewRequest("GET", "", nil)
		transport.RoundTrip(dummy)
		if auth := dummy.Header.Get("Authorization"); auth != "" {
			req.Header.Set("Authorization", auth)
		}
	}

	// Use http.DefaultClient to make the request
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute GraphQL request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GraphQL request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var graphQLResponse struct {
		Data   interface{} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors,omitempty"`
	}
	graphQLResponse.Data = result

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read GraphQL response: %w", err)
	}

	if err := json.Unmarshal(bodyBytes, &graphQLResponse); err != nil {
		return fmt.Errorf("failed to decode GraphQL response: %w", err)
	}

	if len(graphQLResponse.Errors) > 0 {
		return fmt.Errorf("GraphQL errors: %v", graphQLResponse.Errors[0].Message)
	}

	return nil
}

// GraphQL queries for GitHub Projects
const (
	listProjectsQuery = `
	query($org: String!, $first: Int, $after: String) {
		organization(login: $org) {
			projectsV2(first: $first, after: $after) {
				nodes {
					id
					title
					shortDescription
					url
					closed
					number
					items {
						totalCount
					}
				}
				pageInfo {
					hasNextPage
					endCursor
				}
			}
		}
	}`

	addItemToProjectQuery = `
	mutation($projectId: ID!, $contentId: ID!) {
		addProjectV2ItemById(input: {projectId: $projectId, contentId: $contentId}) {
			item {
				id
			}
		}
	}`

	updateProjectItemFieldValueQuery = `
	mutation($projectId: ID!, $itemId: ID!, $fieldId: ID!, $value: ProjectV2FieldValue!) {
		updateProjectV2ItemFieldValue(
			input: {
				projectId: $projectId
				itemId: $itemId
				fieldId: $fieldId
				value: $value
			}
		) {
			projectV2Item {
				id
			}
		}
	}`
)

// Project represents a GitHub Project (V2)
type Project struct {
	ID               string `json:"id"`
	Title            string `json:"title"`
	ShortDescription string `json:"shortDescription"`
	URL              string `json:"url"`
	Closed           bool   `json:"closed"`
	Number           int    `json:"number"`
	ItemCount        int    `json:"itemCount"`
}

// ProjectItem represents an item in a project
type ProjectItem struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	FieldID   string `json:"fieldId,omitempty"`
	ColumnID  string `json:"columnId,omitempty"`
	ContentID string `json:"contentId,omitempty"`
}

// ListOrgProjects creates a tool to list projects in an organization
func ListOrgProjects(getClient GetClientFn, t translations.TranslationHelperFunc) (tool mcp.Tool, handler server.ToolHandlerFunc) {
	return mcp.NewTool("list_org_projects",
			mcp.WithDescription(t("TOOL_LIST_ORG_PROJECTS_DESCRIPTION", "List projects in a GitHub organization using the GraphQL API.")),
			mcp.WithToolAnnotation(mcp.ToolAnnotation{
				Title:        t("TOOL_LIST_ORG_PROJECTS_USER_TITLE", "List organization projects"),
				ReadOnlyHint: true,
			}),
			mcp.WithString("org",
				mcp.Required(),
				mcp.Description("Organization name"),
			),
			mcp.WithString("state",
				mcp.Description("Filter projects by state"),
				mcp.Enum("open", "closed", "all"),
			),
			WithPagination(),
		),
		func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			org, err := requiredParam[string](request, "org")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}

			state, err := OptionalParam[string](request, "state")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}

			pagination, err := OptionalPaginationParams(request)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}

			client, err := getClient(ctx)
			if err != nil {
				return nil, fmt.Errorf("failed to get GitHub client: %w", err)
			}

			variables := map[string]interface{}{
				"org":   org,
				"first": pagination.perPage,
			}

			var response struct {
				Organization struct {
					ProjectsV2 struct {
						Nodes []struct {
							ID               string `json:"id"`
							Title            string `json:"title"`
							ShortDescription string `json:"shortDescription"`
							URL              string `json:"url"`
							Closed           bool   `json:"closed"`
							Number           int    `json:"number"`
							Items            struct {
								TotalCount int `json:"totalCount"`
							} `json:"items"`
						} `json:"nodes"`
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"projectsV2"`
				} `json:"organization"`
			}

			err = executeGraphQL(ctx, client, listProjectsQuery, variables, &response)
			if err != nil {
				return nil, fmt.Errorf("failed to list organization projects: %w", err)
			}

			// Filter projects by state if specified
			projects := make([]Project, 0)
			for _, node := range response.Organization.ProjectsV2.Nodes {
				if state == "" || state == "all" || (state == "closed" && node.Closed) || (state == "open" && !node.Closed) {
					projects = append(projects, Project{
						ID:               node.ID,
						Title:            node.Title,
						ShortDescription: node.ShortDescription,
						URL:              node.URL,
						Closed:           node.Closed,
						Number:           node.Number,
						ItemCount:        node.Items.TotalCount,
					})
				}
			}

			r, err := json.Marshal(projects)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal response: %w", err)
			}

			return mcp.NewToolResultText(string(r)), nil
		}
}

// AddIssueToProject creates a tool to add an issue to a project
func AddIssueToProject(getClient GetClientFn, t translations.TranslationHelperFunc) (tool mcp.Tool, handler server.ToolHandlerFunc) {
	return mcp.NewTool("add_issue_to_project",
			mcp.WithDescription(t("TOOL_ADD_ISSUE_TO_PROJECT_DESCRIPTION", "Add an issue to a GitHub project using GraphQL API.")),
			mcp.WithToolAnnotation(mcp.ToolAnnotation{
				Title:        t("TOOL_ADD_ISSUE_TO_PROJECT_USER_TITLE", "Add issue to project"),
				ReadOnlyHint: false,
			}),
			mcp.WithString("owner",
				mcp.Required(),
				mcp.Description("Repository owner"),
			),
			mcp.WithString("repo",
				mcp.Required(),
				mcp.Description("Repository name"),
			),
			mcp.WithNumber("issue_number",
				mcp.Required(),
				mcp.Description("Issue number to add to project"),
			),
			mcp.WithString("project_id",
				mcp.Required(),
				mcp.Description("Project ID (GraphQL node ID) to add the issue to"),
			),
		),
		func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			projectID, err := requiredParam[string](request, "project_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}

			owner, err := requiredParam[string](request, "owner")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}

			repo, err := requiredParam[string](request, "repo")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}

			issueNumber, err := RequiredInt(request, "issue_number")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}

			client, err := getClient(ctx)
			if err != nil {
				return nil, fmt.Errorf("failed to get GitHub client: %w", err)
			}

			// First get the issue's node ID
			issue, _, err := client.Issues.Get(ctx, owner, repo, issueNumber)
			if err != nil {
				return nil, fmt.Errorf("failed to get issue: %w", err)
			}

			if issue.NodeID == nil {
				return nil, fmt.Errorf("issue node ID is nil")
			}

			variables := map[string]interface{}{
				"projectId": projectID,
				"contentId": *issue.NodeID,
			}

			var response struct {
				AddProjectV2ItemById struct {
					Item struct {
						ID string `json:"id"`
					} `json:"item"`
				} `json:"addProjectV2ItemById"`
			}

			err = executeGraphQL(ctx, client, addItemToProjectQuery, variables, &response)
			if err != nil {
				return nil, fmt.Errorf("failed to add issue to project: %w", err)
			}

			result := ProjectItem{
				ID:        response.AddProjectV2ItemById.Item.ID,
				Type:      "ISSUE",
				ContentID: *issue.NodeID,
			}

			r, err := json.Marshal(result)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal response: %w", err)
			}

			return mcp.NewToolResultText(string(r)), nil
		}
}

// UpdateProjectItemState creates a tool to update a project item's state
func UpdateProjectItemState(getClient GetClientFn, t translations.TranslationHelperFunc) (tool mcp.Tool, handler server.ToolHandlerFunc) {
	return mcp.NewTool("update_project_item_state",
			mcp.WithDescription(t("TOOL_UPDATE_PROJECT_ITEM_STATE_DESCRIPTION", "Update a project item's state using GraphQL API.")),
			mcp.WithToolAnnotation(mcp.ToolAnnotation{
				Title:        t("TOOL_UPDATE_PROJECT_ITEM_STATE_USER_TITLE", "Update project item state"),
				ReadOnlyHint: false,
			}),
			mcp.WithString("project_id",
				mcp.Required(),
				mcp.Description("Project ID (GraphQL node ID)"),
			),
			mcp.WithString("item_id",
				mcp.Required(),
				mcp.Description("Project item ID to update"),
			),
			mcp.WithString("field_id",
				mcp.Required(),
				mcp.Description("Field ID (status/state field) to update"),
			),
			mcp.WithString("value",
				mcp.Required(),
				mcp.Description("New value for the field"),
			),
		),
		func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			projectID, err := requiredParam[string](request, "project_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}

			itemID, err := requiredParam[string](request, "item_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}

			fieldID, err := requiredParam[string](request, "field_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}

			value, err := requiredParam[string](request, "value")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}

			client, err := getClient(ctx)
			if err != nil {
				return nil, fmt.Errorf("failed to get GitHub client: %w", err)
			}

			variables := map[string]interface{}{
				"projectId": projectID,
				"itemId":    itemID,
				"fieldId":   fieldID,
				"value": map[string]interface{}{
					"singleSelectOptionId": value,
				},
			}

			var response struct {
				UpdateProjectV2ItemFieldValue struct {
					ProjectV2Item struct {
						ID string `json:"id"`
					} `json:"projectV2Item"`
				} `json:"updateProjectV2ItemFieldValue"`
			}

			err = executeGraphQL(ctx, client, updateProjectItemFieldValueQuery, variables, &response)
			if err != nil {
				return nil, fmt.Errorf("failed to update project item state: %w", err)
			}

			result := ProjectItem{
				ID:      response.UpdateProjectV2ItemFieldValue.ProjectV2Item.ID,
				FieldID: fieldID,
			}

			r, err := json.Marshal(result)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal response: %w", err)
			}

			return mcp.NewToolResultText(string(r)), nil
		}
}
