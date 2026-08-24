package service

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/yscr/config"
)

func reportsHandler(t *testing.T, home string) http.Handler {
	t.Helper()
	t.Setenv("HOME", home)
	s := &Server{cfg: &config.Config{}, sse: newSSEHub()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/reports", s.handleReports)
	mux.HandleFunc("GET /api/reports/{name}", s.handleReport)
	return mux
}

func TestHandleReports_ListAndFetch(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".yscr", "reports")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "20260101-000000-fleet-status.md"), []byte("# fleet\nok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a report"), 0o644)

	srv := httptest.NewServer(reportsHandler(t, home))
	defer srv.Close()

	r, err := http.Get(srv.URL + "/api/reports")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	body := mustRead(t, r)
	if !strings.Contains(body, "fleet-status.md") || strings.Contains(body, "notes.txt") {
		t.Fatalf("list wrong: %s", body)
	}

	r2, err := http.Get(srv.URL + "/api/reports/20260101-000000-fleet-status.md")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	if r2.StatusCode != 200 || !strings.Contains(mustRead(t, r2), "# fleet") {
		t.Fatalf("fetch failed: %d", r2.StatusCode)
	}
}

func TestHandleReport_RejectsTraversal(t *testing.T) {
	home := t.TempDir()
	srv := httptest.NewServer(reportsHandler(t, home))
	defer srv.Close()

	for _, name := range []string{"..%2F..%2Fetc%2Fpasswd.md", "nope.md"} {
		r, err := http.Get(srv.URL + "/api/reports/" + name)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode == 200 {
			t.Fatalf("%s: expected non-200, got 200", name)
		}
	}
}

func mustRead(t *testing.T, r *http.Response) string {
	t.Helper()
	b := make([]byte, 1<<16)
	n, _ := r.Body.Read(b)
	return string(b[:n])
}
