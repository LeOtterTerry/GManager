package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LeOtterTerry/GManager/agent/internal/config"
	"github.com/LeOtterTerry/GManager/agent/internal/core"
	"github.com/LeOtterTerry/GManager/agent/modules/plainsofpain"
)

func newAPI(t *testing.T, limit int) (*Server, *httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "configs", "server.json")
	ed, err := config.NewEditor(cfgPath, filepath.Join(dir, "state", "server.json.meta"))
	if err != nil {
		t.Fatal(err)
	}
	if limit < 1 {
		limit = 1000
	}
	srv, err := New(Options{
		Token:      "test-token",
		InstanceID: "default",
		Instance:   core.Instance{ID: "default", Dir: dir},
		Module:     plainsofpain.New(),
		Editor:     ed,
		Jobs:       NewJobRunner(true),
		Logs:       NewLogStore(),
		Limiter:    NewLimiter(limit, time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	return srv, httpSrv, cfgPath
}

func do(t *testing.T, base, method, path, token string, body string, headers map[string]string) (int, []byte, http.Header) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, base+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, got, res.Header
}

func writeCfg(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBindAllowsLoopbackOnly(t *testing.T) {
	if err := ValidateAddr("127.0.0.1:8787"); err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{"0.0.0.0:8787", "10.0.0.8:8787", "localhost:8787", ":8787"} {
		if err := ValidateAddr(addr); err == nil {
			t.Fatalf("accepted %s", addr)
		}
	}
}

func TestAuthHostOriginAndRateLimit(t *testing.T) {
	_, httpSrv, _ := newAPI(t, 2)
	status, _, _ := do(t, httpSrv.URL, http.MethodGet, "/api/v1/health", "", "", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("missing token = %d", status)
	}
	status, body, _ := do(t, httpSrv.URL, http.MethodGet, "/api/v1/health", "test-token", "", nil)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"bindHost":"127.0.0.1"`)) {
		t.Fatalf("health = %d %s", status, body)
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodGet, "/api/v1/health", "test-token", "", nil)
	if status != http.StatusOK {
		t.Fatalf("second authenticated request = %d %s", status, body)
	}
	status, body, hdr := do(t, httpSrv.URL, http.MethodGet, "/api/v1/health", "test-token", "", nil)
	if status != http.StatusTooManyRequests {
		t.Fatalf("third authenticated request = %d %s", status, body)
	}
	retryAfter, err := strconv.Atoi(hdr.Get("Retry-After"))
	if err != nil || retryAfter < 1 {
		t.Fatalf("Retry-After = %q", hdr.Get("Retry-After"))
	}

	_, open, _ := newAPI(t, 20)
	req, err := http.NewRequest(http.MethodGet, open.URL+"/api/v1/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "evil.example"
	req.Header.Set("Authorization", "Bearer test-token")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign host = %d", res.StatusCode)
	}

	status, _, _ = do(t, open.URL, http.MethodGet, "/api/v1/health", "test-token", "", map[string]string{"Origin": "https://evil.example"})
	if status != http.StatusForbidden {
		t.Fatalf("foreign origin = %d", status)
	}
	status, _, _ = do(t, open.URL, http.MethodGet, "/api/v1/health", "test-token", "", map[string]string{"Origin": "http://127.0.0.1:5173"})
	if status != http.StatusOK {
		t.Fatalf("local origin = %d", status)
	}
	status, _, _ = do(t, open.URL, http.MethodGet, "/api/v1/health", "test-token", "", map[string]string{"Origin": "http://127.0.0.1:8787"})
	if status != http.StatusForbidden {
		t.Fatalf("api-port origin = %d", status)
	}
	status, _, _ = do(t, open.URL, http.MethodGet, "/api/v1/health", "short", "", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("short token = %d", status)
	}
}

func TestInstanceID(t *testing.T) {
	for _, id := range []string{"default", "A", "server_1", "a-b"} {
		if err := ValidateInstanceID(id); err != nil {
			t.Errorf("ValidateInstanceID(%q) = %v", id, err)
		}
	}
	for _, id := range []string{"", "..", "a/b", "a\\b", "-lead", ".hidden", "has space", strings.Repeat("a", 65)} {
		if err := ValidateInstanceID(id); err == nil {
			t.Errorf("ValidateInstanceID(%q) accepted", id)
		}
	}
}

func TestAsyncContractJobsConfirmAndLogs(t *testing.T) {
	srv, httpSrv, cfgPath := newAPI(t, 100)
	writeCfg(t, cfgPath, "{\"serverName\":\"kept\"}\n")
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	status, body, hdr := do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/start", "test-token", `{}`, map[string]string{"Idempotency-Key": "start-1"})
	if status != http.StatusAccepted {
		t.Fatalf("start = %d %s", status, body)
	}
	jobID := jsonString(t, body, "jobId")
	if hdr.Get("Location") != "/api/v1/jobs/"+jobID {
		t.Fatalf("location = %s", hdr.Get("Location"))
	}
	status, again, _ := do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/start", "test-token", `{}`, map[string]string{"Idempotency-Key": "start-1"})
	if status != http.StatusAccepted || jsonString(t, again, "jobId") != jobID {
		t.Fatalf("replay = %d %s", status, again)
	}
	status, conflict, _ := do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/start", "test-token", `{"other":true}`, map[string]string{"Idempotency-Key": "start-1"})
	if status != http.StatusConflict || !bytes.Contains(conflict, []byte("idempotency_conflict")) {
		t.Fatalf("reused key = %d %s", status, conflict)
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("start rewrote config\n%s", after)
	}

	status, jobBody, _ := do(t, httpSrv.URL, http.MethodGet, "/api/v1/jobs/"+jobID, "test-token", "", nil)
	if status != http.StatusOK {
		t.Fatalf("job = %d %s", status, jobBody)
	}
	var job struct {
		State  string          `json:"state"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(jobBody, &job); err != nil {
		t.Fatal(err)
	}
	if job.State != string(JobSucceeded) {
		t.Fatalf("state = %s", job.State)
	}
	var result struct {
		ProcessStarted bool     `json:"processStarted"`
		Args           []string `json:"args"`
	}
	if err := json.Unmarshal(job.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.ProcessStarted {
		t.Fatal("start claimed a process")
	}
	if !contains(result.Args, "-nographics") || contains(result.Args, "-cc") {
		t.Fatalf("args = %v", result.Args)
	}

	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/stop", "test-token", `{}`, nil)
	if status != http.StatusBadRequest || !bytes.Contains(body, []byte("confirm_required")) {
		t.Fatalf("stop without confirm = %d %s", status, body)
	}
	status, _, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/restore", "test-token", `{"confirm":false}`, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("restore without confirm = %d", status)
	}
	status, _, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/stop", "test-token", `{"confirm":true}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("confirmed stop = %d", status)
	}

	srv.Logs().Append("alpha")
	srv.Logs().Append("beta")
	srv.Logs().Append("gamma")
	status, body, _ = do(t, httpSrv.URL, http.MethodGet, "/api/v1/servers/default/logs?limit=2", "test-token", "", nil)
	if status != http.StatusOK {
		t.Fatalf("logs = %d %s", status, body)
	}
	var page struct {
		Items []struct {
			Cursor uint64 `json:"cursor"`
			Text   string `json:"text"`
		} `json:"items"`
		Next uint64 `json:"nextCursor"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].Text != "alpha" || page.Items[1].Text != "beta" || page.Next != 2 {
		t.Fatalf("page = %+v", page)
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodGet, "/api/v1/servers/default/logs?cursor=2&limit=2", "test-token", "", nil)
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || len(page.Items) != 1 || page.Items[0].Text != "gamma" || page.Next != 3 {
		t.Fatalf("next page = %d %+v", status, page)
	}
	status, _, _ = do(t, httpSrv.URL, http.MethodGet, "/api/v1/jobs/missing", "test-token", "", nil)
	if status != http.StatusNotFound {
		t.Fatalf("missing job = %d", status)
	}
}

func TestConfigRevisionAndExternalEdit(t *testing.T) {
	_, httpSrv, cfgPath := newAPI(t, 100)
	original := "{\"serverName\":\"A\",\"password\":\"secret\",\"modSetting\":7}\n"
	writeCfg(t, cfgPath, original)

	status, body, _ := do(t, httpSrv.URL, http.MethodGet, "/api/v1/servers/default/config", "test-token", "", nil)
	if status != http.StatusOK {
		t.Fatalf("get = %d %s", status, body)
	}
	var view struct {
		Revision int64 `json:"revision"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	if view.Revision != 1 {
		t.Fatalf("revision = %d", view.Revision)
	}

	status, body, _ = do(t, httpSrv.URL, http.MethodPut, "/api/v1/servers/default/config", "test-token",
		`{"content":{"serverName":"A","password":"this-password-is-too-long"}}`,
		map[string]string{"If-Match": "1"})
	if status != http.StatusBadRequest || !bytes.Contains(body, []byte("validation_failed")) {
		t.Fatalf("bad password = %d %s", status, body)
	}
	if got := mustRead(t, cfgPath); got != original {
		t.Fatalf("invalid save wrote %s", got)
	}

	status, body, _ = do(t, httpSrv.URL, http.MethodPut, "/api/v1/servers/default/config", "test-token",
		`{"content":{"serverName":"Renamed","password":"secret"}}`,
		map[string]string{"If-Match": "1", "Idempotency-Key": "cfg-1"})
	if status != http.StatusAccepted {
		t.Fatalf("save = %d %s", status, body)
	}
	firstJob := jsonString(t, body, "jobId")
	got := mustRead(t, cfgPath)
	if !strings.Contains(got, "Renamed") || !strings.Contains(got, "modSetting") {
		t.Fatalf("saved file dropped a key or the edit: %s", got)
	}
	status, replay, _ := do(t, httpSrv.URL, http.MethodPut, "/api/v1/servers/default/config", "test-token",
		`{"content":{"serverName":"Renamed","password":"secret"}}`,
		map[string]string{"If-Match": "1", "Idempotency-Key": "cfg-1"})
	if status != http.StatusAccepted || jsonString(t, replay, "jobId") != firstJob {
		t.Fatalf("config replay = %d %s", status, replay)
	}

	status, body, _ = do(t, httpSrv.URL, http.MethodGet, "/api/v1/servers/default/config", "test-token", "", nil)
	if status != http.StatusOK {
		t.Fatalf("reread = %d %s", status, body)
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	external := "{\"serverName\":\"Manual\",\"password\":\"secret\",\"modSetting\":7}\n"
	writeCfg(t, cfgPath, external)
	status, body, _ = do(t, httpSrv.URL, http.MethodPut, "/api/v1/servers/default/config", "test-token",
		`{"content":{"serverName":"FromUI","password":"secret"}}`,
		map[string]string{"If-Match": itoa(view.Revision)})
	if status != http.StatusConflict || !bytes.Contains(body, []byte("stale_revision")) {
		t.Fatalf("stale = %d %s", status, body)
	}
	if mustRead(t, cfgPath) != external {
		t.Fatalf("stale save overwrote manual edit: %s", mustRead(t, cfgPath))
	}

	status, body, _ = do(t, httpSrv.URL, http.MethodGet, "/api/v1/servers/default/config", "test-token", "", nil)
	if status != http.StatusOK {
		t.Fatalf("external read = %d %s", status, body)
	}
	var externalView struct {
		Revision       int64 `json:"revision"`
		ExternalChange bool  `json:"externalChange"`
	}
	if err := json.Unmarshal(body, &externalView); err != nil {
		t.Fatal(err)
	}
	if !externalView.ExternalChange {
		t.Fatalf("external flag missing: %s", body)
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodPut, "/api/v1/servers/default/config", "test-token",
		`{"content":{"serverName":"FromUI","password":"secret"}}`,
		map[string]string{"If-Match": itoa(externalView.Revision)})
	if status != http.StatusConflict || !bytes.Contains(body, []byte(`"field":"serverName"`)) {
		t.Fatalf("conflict = %d %s", status, body)
	}
	if mustRead(t, cfgPath) != external {
		t.Fatalf("conflict save overwrote manual edit: %s", mustRead(t, cfgPath))
	}
}

func TestJobRunnerIsAsync(t *testing.T) {
	runner := NewJobRunner(false)
	started := make(chan struct{})
	release := make(chan struct{})
	job := runner.Start("start", "default", func() (any, error) {
		close(started)
		<-release
		return map[string]bool{"ok": true}, nil
	})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("job did not start")
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, ok := runner.Get(job.ID)
		if ok && got.State == JobSucceeded {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job did not succeed")
}

func jsonString(t *testing.T, body []byte, key string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	s, _ := m[key].(string)
	if s == "" {
		t.Fatalf("missing %s in %s", key, body)
	}
	return s
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
