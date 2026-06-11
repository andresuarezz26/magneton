package jira

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Search runs a JQL query and returns matching issues (Decision 5).
func (c *Client) Search(jql string, maxResults int) ([]Issue, error) {
	if maxResults <= 0 {
		maxResults = 50
	}
	u := fmt.Sprintf("%s/rest/api/2/search?jql=%s&fields=summary,description&maxResults=%d",
		c.BaseURL, url.QueryEscape(jql), maxResults)
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	c.auth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jira search: HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	var raw struct {
		Issues []struct {
			Key    string `json:"key"`
			Fields struct {
				Summary     string          `json:"summary"`
				Description json.RawMessage `json:"description"`
			} `json:"fields"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	out := make([]Issue, 0, len(raw.Issues))
	for _, it := range raw.Issues {
		out = append(out, Issue{
			Key:         it.Key,
			Summary:     it.Fields.Summary,
			Description: flattenDesc(it.Fields.Description),
		})
	}
	return out, nil
}

// Transition moves an issue to the named status (matches the transition name or
// its destination status name, case-insensitive). No-op if already there.
func (c *Client) Transition(key, target string) error {
	u := fmt.Sprintf("%s/rest/api/2/issue/%s/transitions", c.BaseURL, url.PathEscape(key))
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	c.auth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jira transitions %s: HTTP %d: %s", key, resp.StatusCode, truncate(string(body), 200))
	}
	var raw struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			To   struct {
				Name string `json:"name"`
			} `json:"to"`
		} `json:"transitions"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return err
	}
	var id string
	for _, tr := range raw.Transitions {
		if strings.EqualFold(tr.Name, target) || strings.EqualFold(tr.To.Name, target) {
			id = tr.ID
			break
		}
	}
	if id == "" {
		return fmt.Errorf("no transition to %q available for %s", target, key)
	}
	payload := fmt.Sprintf(`{"transition":{"id":%q}}`, id)
	preq, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(payload))
	c.auth(preq)
	presp, err := c.http.Do(preq)
	if err != nil {
		return err
	}
	defer presp.Body.Close()
	if presp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(presp.Body)
		return fmt.Errorf("jira transition %s: HTTP %d: %s", key, presp.StatusCode, truncate(string(b), 200))
	}
	return nil
}
