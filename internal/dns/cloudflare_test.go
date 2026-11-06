package dns

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeCloudflare(t *testing.T) (*Client, *[]string) {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing auth header")
		}
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/zones":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": []map[string]string{{"id": "z1", "name": r.URL.Query().Get("name")}}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/dns_records"):
			page := r.URL.Query().Get("page")
			recs := []Record{{ID: "a", Name: "x.r1.test.slicedbread.dev"}, {ID: "b", Name: "www.slicedbread.dev"}}
			if page == "2" {
				recs = []Record{{ID: "c", Name: "test.slicedbread.dev"}}
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": recs, "result_info": map[string]int{"page": 1, "total_pages": 2}})
		case r.Method == http.MethodPost:
			var rec Record
			json.NewDecoder(r.Body).Decode(&rec)
			rec.ID = "new"
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": rec})
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/gone"):
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]any{{"code": 81044, "message": "Record does not exist."}}})
		case r.Method == http.MethodDelete:
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]string{"id": "x"}})
		}
	}))
	t.Cleanup(srv.Close)
	c := New("tok")
	c.BaseURL = srv.URL
	return c, &calls
}

func TestClient(t *testing.T) {
	c, _ := fakeCloudflare(t)
	ctx := context.Background()
	zid, err := c.ZoneID(ctx, "slicedbread.dev")
	if err != nil || zid != "z1" {
		t.Fatalf("ZoneID = %q, %v", zid, err)
	}
	rec, err := c.Create(ctx, zid, Record{Type: "A", Name: "*.r1.test.slicedbread.dev", Content: "1.2.3.4", TTL: 60})
	if err != nil || rec.ID != "new" || rec.Content != "1.2.3.4" {
		t.Fatalf("Create = %+v, %v", rec, err)
	}
	recs, err := c.ListUnder(ctx, zid, "test.slicedbread.dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].ID != "a" || recs[1].ID != "c" {
		t.Fatalf("ListUnder must filter by suffix across pages, got %+v", recs)
	}
	if err := c.Delete(ctx, zid, "gone"); err != nil {
		t.Fatalf("deleting a missing record should succeed: %v", err)
	}
	if err := c.Delete(ctx, zid, "a"); err != nil {
		t.Fatal(err)
	}
}
