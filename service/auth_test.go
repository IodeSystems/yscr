package service

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func authTestServer(token string) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/fleet", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"sessions":[]}`)) })
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("shell")) }))
	if token != "" {
		return httptest.NewServer(authMiddleware(token, mux))
	}
	return httptest.NewServer(mux)
}

func TestAuth_Off_AllowsAll(t *testing.T) {
	srv := authTestServer("")
	defer srv.Close()
	r, err := http.Get(srv.URL + "/api/fleet")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("auth off: status %d", r.StatusCode)
	}
}

func TestAuth_On_RequiresToken(t *testing.T) {
	srv := authTestServer("s3cret")
	defer srv.Close()

	r, err := http.Get(srv.URL + "/api/fleet")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 401 {
		t.Fatalf("no token: status %d, want 401", r.StatusCode)
	}

	req, _ := http.NewRequest("GET", srv.URL+"/api/fleet", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != 401 {
		t.Fatalf("wrong token: status %d, want 401", r2.StatusCode)
	}

	req3, _ := http.NewRequest("GET", srv.URL+"/api/fleet", nil)
	req3.Header.Set("Authorization", "bearer s3cret") // scheme case-insensitive
	r3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	r3.Body.Close()
	if r3.StatusCode != 200 {
		t.Fatalf("good token: status %d, want 200", r3.StatusCode)
	}
}

func TestAuth_On_ShellStaysOpen(t *testing.T) {
	srv := authTestServer("s3cret")
	defer srv.Close()
	r, err := http.Get(srv.URL + "/index.html")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("shell: status %d, want 200", r.StatusCode)
	}
}
