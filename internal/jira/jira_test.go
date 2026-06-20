package jira

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSetDescription(t *testing.T) {
	var gotMethod, gotPath, gotDesc string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Fields struct {
				Description string `json:"description"`
			} `json:"fields"`
		}
		_ = json.Unmarshal(body, &payload)
		gotDesc = payload.Fields.Description
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(srv.URL, "me@x.com", "tok")
	if err := c.SetDescription("KAN-6", "answer text"); err != nil {
		t.Fatalf("SetDescription: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %s, want PUT", gotMethod)
	}
	if gotPath != "/rest/api/2/issue/KAN-6" {
		t.Errorf("path = %s", gotPath)
	}
	if gotDesc != "answer text" {
		t.Errorf("description = %q", gotDesc)
	}
}

func TestSetDescriptionError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errorMessages":["nope"]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "me@x.com", "tok")
	if err := c.SetDescription("KAN-6", "x"); err == nil {
		t.Error("expected error on HTTP 403")
	}
}

func TestFlattenDesc(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain string", `"just text"`, "just text"},
		{"null", `null`, ""},
		{"empty", ``, ""},
		{
			"adf",
			`{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Bump AGP"},{"type":"text","text":" to 8.5"}]}]}`,
			"Bump AGP to 8.5",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := flattenDesc([]byte(c.in)); got != c.want {
				t.Fatalf("flattenDesc(%s) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
