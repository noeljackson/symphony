// Package github implements the [tracker.Tracker] interface against the
// GitHub Issues REST API per SPEC v3 §5.3.1.B.
//
// One Client instance owns one repository's tracker view. Auth is via a
// Personal Access Token in the `Authorization: Bearer …` header; GitHub
// App installation auth is reserved for a future PR.
//
// State-mapping rules (SPEC §5.3.1.B):
//
//   - The configured ActiveStates / TerminalStates match against issue
//     **labels**, not the built-in `open` / `closed` state.
//   - When no label matches, the literal `open` / `closed` is used as a
//     fallback (so a `closed` issue is always terminal even without a
//     matching label).
//   - Pull requests are filtered out — `GET /repos/{owner}/{repo}/issues`
//     returns both issues and PRs by default.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/noeljackson/symphony/go/internal/issue"
)

// Config configures a GitHub Client.
type Config struct {
	Endpoint         string
	Owner            string
	Repo             string
	APIToken         string
	LabelPriorityMap map[string]int
	Assignee         string
	ActiveStates     []string
	TerminalStates   []string
	HTTPClient       *http.Client
	PerPage          int
}

// Client is a GitHub Issues Tracker implementation.
type Client struct {
	cfg     Config
	http    *http.Client
	perPage int
}

// New constructs a Client from cfg.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.Owner) == "" {
		return nil, fmt.Errorf("github.New: Owner is required")
	}
	if strings.TrimSpace(cfg.Repo) == "" {
		return nil, fmt.Errorf("github.New: Repo is required")
	}
	if strings.TrimSpace(cfg.APIToken) == "" {
		return nil, fmt.Errorf("github.New: APIToken is required (GitHub App auth is not yet supported)")
	}
	if strings.TrimSpace(cfg.Endpoint) == "" {
		cfg.Endpoint = "https://api.github.com"
	}
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	pp := cfg.PerPage
	if pp <= 0 {
		pp = 100
	}
	if pp > 100 {
		pp = 100 // GitHub caps per_page at 100.
	}
	return &Client{cfg: cfg, http: hc, perPage: pp}, nil
}

// FetchCandidateIssues implements [tracker.Tracker].
func (c *Client) FetchCandidateIssues(ctx context.Context) ([]issue.Issue, error) {
	all, err := c.fetchIssues(ctx, "all")
	if err != nil {
		return nil, err
	}
	return filterByStates(all, c.cfg.ActiveStates), nil
}

// FetchIssueStatesByIDs implements [tracker.Tracker].
func (c *Client) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]issue.Issue, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	all, err := c.fetchIssues(ctx, "all")
	if err != nil {
		return nil, err
	}
	want := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	out := make([]issue.Issue, 0, len(ids))
	for _, i := range all {
		if _, ok := want[i.ID]; ok {
			out = append(out, i)
		}
	}
	return out, nil
}

// FetchIssuesByStates implements [tracker.Tracker].
func (c *Client) FetchIssuesByStates(ctx context.Context, states []string) ([]issue.Issue, error) {
	all, err := c.fetchIssues(ctx, "all")
	if err != nil {
		return nil, err
	}
	return filterByStates(all, states), nil
}

func filterByStates(in []issue.Issue, states []string) []issue.Issue {
	if len(states) == 0 {
		return nil
	}
	out := make([]issue.Issue, 0, len(in))
	for _, i := range in {
		for _, s := range states {
			if strings.EqualFold(i.State, s) {
				out = append(out, i)
				break
			}
		}
	}
	return out
}

// issueResponse mirrors the subset of GitHub's REST issue payload we need.
type issueResponse struct {
	NodeID      string             `json:"node_id"`
	Number      int                `json:"number"`
	Title       string             `json:"title"`
	Body        *string            `json:"body"`
	State       string             `json:"state"`
	Labels      []labelResponse    `json:"labels"`
	Assignees   []assigneeResponse `json:"assignees"`
	HTMLURL     string             `json:"html_url"`
	CreatedAt   *time.Time         `json:"created_at"`
	UpdatedAt   *time.Time         `json:"updated_at"`
	PullRequest map[string]any     `json:"pull_request,omitempty"`
}

type labelResponse struct {
	Name string `json:"name"`
}

type assigneeResponse struct {
	Login string `json:"login"`
}

// fetchIssues performs a single page request. ghState is one of "open",
// "closed", or "all"; we always pass "all" so dispatched issues that
// transition to closed mid-cycle are visible during reconciliation.
func (c *Client) fetchIssues(ctx context.Context, ghState string) ([]issue.Issue, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/issues",
		c.cfg.Endpoint,
		url.PathEscape(c.cfg.Owner),
		url.PathEscape(c.cfg.Repo))
	q := url.Values{}
	q.Set("state", ghState)
	q.Set("per_page", strconv.Itoa(c.perPage))
	if strings.TrimSpace(c.cfg.Assignee) != "" {
		q.Set("assignee", c.cfg.Assignee)
	}
	u = u + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIToken)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("github http %d: %s", resp.StatusCode, truncate(string(raw), 256))
	}
	var nodes []issueResponse
	if err := json.Unmarshal(raw, &nodes); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	out := make([]issue.Issue, 0, len(nodes))
	for _, n := range nodes {
		if n.PullRequest != nil {
			continue // skip PRs surfaced by GET /issues
		}
		out = append(out, c.project(n))
	}
	return out, nil
}

func (c *Client) project(n issueResponse) issue.Issue {
	id := fmt.Sprintf("%s/%s/issues/%d", c.cfg.Owner, c.cfg.Repo, n.Number)
	identifier := fmt.Sprintf("%s/%s#%d", c.cfg.Owner, c.cfg.Repo, n.Number)
	branch := branchName(n.Number, n.Title)
	htmlURL := n.HTMLURL
	state := c.mapState(n)
	i := issue.Issue{
		ID:          id,
		Identifier:  identifier,
		Title:       n.Title,
		Description: n.Body,
		State:       state,
		BranchName:  &branch,
		URL:         &htmlURL,
		CreatedAt:   n.CreatedAt,
		UpdatedAt:   n.UpdatedAt,
	}
	if pri := c.priorityFromLabels(n.Labels); pri != nil {
		i.Priority = pri
	}
	for _, l := range n.Labels {
		i.Labels = append(i.Labels, l.Name)
	}
	return i
}

// mapState applies the SPEC §5.3.1.B label-as-state contract:
// first matching label wins; closed issues are always terminal even
// without a matching label; otherwise the literal `open` / `closed` is
// used as a fallback.
func (c *Client) mapState(n issueResponse) string {
	for _, l := range n.Labels {
		if anyEqualFold(c.cfg.ActiveStates, l.Name) {
			return l.Name
		}
		if anyEqualFold(c.cfg.TerminalStates, l.Name) {
			return l.Name
		}
	}
	if strings.EqualFold(n.State, "closed") {
		return "closed"
	}
	return "open"
}

// priorityFromLabels picks the lowest (best) priority from any label that
// appears in LabelPriorityMap. Issues without a matching label get nil
// (default priority handled by dispatch_key in §16.2).
func (c *Client) priorityFromLabels(labels []labelResponse) *int {
	if len(c.cfg.LabelPriorityMap) == 0 {
		return nil
	}
	var picked *int
	for _, l := range labels {
		if v, ok := c.cfg.LabelPriorityMap[l.Name]; ok {
			if picked == nil || v < *picked {
				p := v
				picked = &p
			}
		}
	}
	return picked
}

// branchName produces a slugified branch name per SPEC §5.3.1.B
// recommendation: `<number>-<slug-of-title>`.
func branchName(number int, title string) string {
	const maxSlug = 50
	var b strings.Builder
	prev := byte('-')
	for i := 0; i < len(title) && b.Len() < maxSlug; i++ {
		ch := title[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9':
			b.WriteByte(ch)
			prev = ch
		case ch >= 'A' && ch <= 'Z':
			b.WriteByte(ch + 32)
			prev = ch + 32
		default:
			if prev != '-' {
				b.WriteByte('-')
				prev = '-'
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return strconv.Itoa(number)
	}
	return strconv.Itoa(number) + "-" + slug
}

func anyEqualFold(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.EqualFold(s, needle) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
