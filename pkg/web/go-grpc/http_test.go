package go_grpc

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandlerServesConnectUnary exercises a Connect unary call over the exact
// wire shape the dashboard's fetch client uses (POST, application/json,
// Connect-Protocol-Version: 1, JSON body) against the real connect-go handler,
// so a mismatch in that contract is caught in Go, not only in the browser.
func TestHandlerServesConnectUnary(t *testing.T) {
	server, err := NewServer(&Configuration{EndpointGrpc: "127.0.0.1:0"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := (&HttpServer{config: &Configuration{}, impl: server}).handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/codefly.cli.v0.CLI/Ping", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("Ping over Connect: status %d, body %s", res.StatusCode, body)
	}
	if strings.TrimSpace(string(body)) != "{}" {
		t.Fatalf("Ping over Connect: unexpected body %q", body)
	}

	graphServer, err := NewServer(&Configuration{EndpointGrpc: "127.0.0.1:0"}, loadGraphWorkspace(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	graphHandler, err := (&HttpServer{config: &Configuration{}, impl: graphServer}).handler()
	if err != nil {
		t.Fatal(err)
	}
	graphSrv := httptest.NewServer(graphHandler)
	defer graphSrv.Close()

	graphReq, err := http.NewRequest(http.MethodPost, graphSrv.URL+"/codefly.cli.v0.CLI/GetWorkspaceServiceDependencyGraph", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	graphReq.Header.Set("Content-Type", "application/json")
	graphReq.Header.Set("Connect-Protocol-Version", "1")

	graphRes, err := http.DefaultClient.Do(graphReq)
	if err != nil {
		t.Fatal(err)
	}
	defer graphRes.Body.Close()
	graphBody, _ := io.ReadAll(graphRes.Body)
	if graphRes.StatusCode != http.StatusOK {
		t.Fatalf("GetWorkspaceServiceDependencyGraph over Connect: status %d, body %s", graphRes.StatusCode, graphBody)
	}
	if !strings.Contains(string(graphBody), "backend/api") {
		t.Fatalf("GetWorkspaceServiceDependencyGraph over Connect: body missing %q; got %s", "backend/api", graphBody)
	}
}

// TestHandlerServesDashboard verifies the embedded dashboard build is served at
// "/" and that its hashed asset bundle is reachable, so the go:embed wiring and
// the SPA output stay in sync.
func TestHandlerServesDashboard(t *testing.T) {
	h, err := (&HttpServer{config: &Configuration{}}).handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := get(t, srv.URL+"/")
	if !strings.Contains(body, `id="root"`) {
		t.Fatalf("dashboard index.html missing SPA root element; got:\n%s", body)
	}
	if !strings.Contains(body, "/assets/") {
		t.Fatalf("dashboard index.html does not reference a built asset bundle; got:\n%s", body)
	}
}

func get(t *testing.T, url string) string {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, res.StatusCode)
	}
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
