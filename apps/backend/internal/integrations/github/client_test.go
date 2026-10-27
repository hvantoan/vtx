package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vtx/platform/internal/application"
)

func TestCreateDraftPRHardcodesDraftAndPreservesEvidenceBody(t *testing.T) {
	var got map[string]any
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/acme/repo/pulls" {
			t.Errorf("request=%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing bearer header")
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":99,"number":12,"html_url":"https://github.com/acme/repo/pull/12","draft":true,"state":"open","head":{"sha":"abc123"}}`))
	}))
	defer server.Close()
	client, err := NewClient("https://api.github.com", "test-token")
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	client.http = server.Client()
	_, err = client.CreateDraftPR(context.Background(), "acme", "repo", application.CreatePRInput{Title: "title", Body: "run-1 sha abc123", Head: "feature", Base: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if got["draft"] != true {
		t.Fatalf("draft=%v, want true", got["draft"])
	}
	if got["base"] != "dev" || got["head"] != "feature" || got["title"] != "title" || got["body"] != "run-1 sha abc123" {
		t.Fatalf("body=%#v", got)
	}
}
