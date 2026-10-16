package panel

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// The panel passes API calls to the agent's socket without the browser's credentials,
// and says "unavailable" in Connect's terms when no agent listens.
func TestAgentAPI(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "a.sock")
	h := AgentAPI(sock)

	req := httptest.NewRequest(http.MethodPost, "/devopus.agent.v1.AgentService/Status", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"code":"unavailable"`) {
		t.Fatalf("no agent: %d %s", rec.Code, rec.Body)
	}

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var got *http.Request
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		_, _ = io.WriteString(w, `{"agentVersion":"x"}`)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	req = httptest.NewRequest(http.MethodPost, "/devopus.agent.v1.AgentService/Status", strings.NewReader("{}"))
	req.Header.Set("Cookie", "devopus_panel_x=secret")
	req.Header.Set(TokenHeader, "Bearer secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != `{"agentVersion":"x"}` {
		t.Fatalf("with an agent: %d %s", rec.Code, rec.Body)
	}
	if got.URL.Path != "/devopus.agent.v1.AgentService/Status" || got.Header.Get("Cookie") != "" || got.Header.Get(TokenHeader) != "" {
		t.Fatalf("forwarded %s with cookie %q, auth %q", got.URL.Path, got.Header.Get("Cookie"), got.Header.Get(TokenHeader))
	}
}
