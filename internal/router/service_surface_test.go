package router

// Tests for the service-surface hardening: API-key uid binding (a key-pinned
// owner overrides the client-supplied X-Uid/body uid), executor_url
// validation (scheme whitelist + optional private-host blocking), the
// {error:{code,message}} envelope, 201+Location on register, and /tasks list
// pagination.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"app-task/internal/config"
	"app-task/internal/repo"
)

// createBoundKey mints and persists a key row pinned to uid (0 = unbound),
// returning the plaintext for request headers.
func createBoundKey(t *testing.T, name string, uid int64) string {
	t.Helper()
	plaintext := "at_bound_" + name + "_0123456789abcdef"
	row := mkRow(name, plaintext)
	row.UID = uid
	if err := repo.CreateAPIKey(&row); err != nil {
		t.Fatalf("create key: %v", err)
	}
	return plaintext
}

// errCode extracts error.code from the response envelope.
func errCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("envelope: %v (body = %s)", err, w.Body.String())
	}
	return resp.Error.Code
}

// listResp is the /tasks response shape the tests assert on.
type listResp struct {
	Tasks  []struct {
		TaskID string `json:"task_id"`
	} `json:"tasks"`
	Total  int64 `json:"total"`
	Limit  int   `json:"limit"`
	Offset int   `json:"offset"`
}

func TestBoundUIDKeyEnforcement(t *testing.T) {
	_, h := newTestRouter(t)
	key := createBoundKey(t, "bound7", 7)
	hdr := map[string]string{"X-API-Key": key}

	// register with a mismatched body uid → 403 forbidden
	mismatch := `{"uid":9,"task_type":"http","payload":{},"executor_url":"http://exec:9000","trigger_time":"2030-01-01T00:00:00Z"}`
	if w := doJSON(h, "POST", "/tasks/register", mismatch, hdr); w.Code != http.StatusForbidden {
		t.Fatalf("mismatched uid: status = %d, body = %s", w.Code, w.Body.String())
	} else if code := errCode(t, w); code != "forbidden" {
		t.Fatalf("mismatched uid: code = %q, want forbidden", code)
	}

	// matching uid → 201
	matching := `{"uid":7,"task_type":"http","payload":{},"executor_url":"http://exec:9000","trigger_time":"2030-01-01T00:00:00Z"}`
	if w := doJSON(h, "POST", "/tasks/register", matching, hdr); w.Code != http.StatusCreated {
		t.Fatalf("matching uid: status = %d, body = %s", w.Code, w.Body.String())
	}

	// list: bound uid applies, no X-Uid needed
	w := doJSON(h, "GET", "/tasks", "", hdr)
	if w.Code != http.StatusOK {
		t.Fatalf("list: status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp listResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("list body: %v", err)
	}
	if resp.Total != 1 || len(resp.Tasks) != 1 {
		t.Fatalf("list total = %d, tasks = %d, want 1/1", resp.Total, len(resp.Tasks))
	}

	// a spoofed X-Uid no longer grants cross-uid access: the bound uid wins
	// and the same uid-7 task set is returned.
	w = doJSON(h, "GET", "/tasks", "", map[string]string{"X-API-Key": key, "X-Uid": "999"})
	if w.Code != http.StatusOK {
		t.Fatalf("spoofed X-Uid list: status = %d", w.Code)
	}
	resp = listResp{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("spoofed list body: %v", err)
	}
	if resp.Total != 1 {
		t.Fatalf("spoofed X-Uid total = %d, want 1 (bound uid wins)", resp.Total)
	}
}

func TestExecutorURLSchemeWhitelist(t *testing.T) {
	_, h := newTestRouter(t)
	hdr := map[string]string{"apikey": testServiceKey}
	for _, raw := range []string{
		"ftp://exec:9000/job",
		"file:///etc/passwd",
		"http://",
		"//exec:9000/job",
	} {
		body := fmt.Sprintf(`{"uid":1,"task_type":"http","payload":{},"executor_url":%q,"trigger_time":"2030-01-01T00:00:00Z"}`, raw)
		w := doJSON(h, "POST", "/tasks/register", body, hdr)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("executor_url %q: status = %d, want 400", raw, w.Code)
		}
		if code := errCode(t, w); code != "invalid_executor_url" {
			t.Fatalf("executor_url %q: code = %q, want invalid_executor_url", raw, code)
		}
	}
	// a well-formed http URL passes
	ok := `{"uid":1,"task_type":"http","payload":{},"executor_url":"http://exec:9000/job","trigger_time":"2030-01-01T00:00:00Z"}`
	if w := doJSON(h, "POST", "/tasks/register", ok, hdr); w.Code != http.StatusCreated {
		t.Fatalf("valid executor_url: status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestExecutorURLPrivateBlocking(t *testing.T) {
	_, h := newTestRouterWithCfg(t, func(cfg *config.Config) {
		cfg.Security.BlockPrivateExecutorHosts = true
	})
	hdr := map[string]string{"apikey": testServiceKey}
	for _, raw := range []string{
		"http://127.0.0.1:9000/job",
		"http://192.168.1.5:9000/job",
		"http://10.0.0.3/job",
		"http://172.16.0.9/job",
		"http://169.254.169.254/latest/meta-data",
		"http://[::1]:9000/job",
		"http://[fd00::1]/job",
	} {
		body := fmt.Sprintf(`{"uid":1,"task_type":"http","payload":{},"executor_url":%q,"trigger_time":"2030-01-01T00:00:00Z"}`, raw)
		w := doJSON(h, "POST", "/tasks/register", body, hdr)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("private host %q: status = %d, want 400", raw, w.Code)
		}
	}
	// a public IP literal passes (IP path needs no DNS)
	pub := `{"uid":1,"task_type":"http","payload":{},"executor_url":"http://93.184.216.34:9000/job","trigger_time":"2030-01-01T00:00:00Z"}`
	if w := doJSON(h, "POST", "/tasks/register", pub, hdr); w.Code != http.StatusCreated {
		t.Fatalf("public IP: status = %d, body = %s", w.Code, w.Body.String())
	}
	// an unresolvable hostname fails closed
	nx := `{"uid":1,"task_type":"http","payload":{},"executor_url":"http://no-such-host-for-tests.invalid/job","trigger_time":"2030-01-01T00:00:00Z"}`
	if w := doJSON(h, "POST", "/tasks/register", nx, hdr); w.Code != http.StatusBadRequest {
		t.Fatalf("unresolvable host: status = %d, want 400 (fail closed)", w.Code)
	}

	// default (flag off): private targets keep working — in-cluster executors
	_, h2 := newTestRouter(t)
	priv := `{"uid":1,"task_type":"http","payload":{},"executor_url":"http://127.0.0.1:9000/job","trigger_time":"2030-01-01T00:00:00Z"}`
	if w := doJSON(h2, "POST", "/tasks/register", priv, hdr); w.Code != http.StatusCreated {
		t.Fatalf("flag off private host: status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestRegisterCreatedAndLocation(t *testing.T) {
	_, h := newTestRouter(t)
	hdr := map[string]string{"apikey": testServiceKey}
	w := doJSON(h, "POST", "/tasks/register", registerBody, hdr)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.TaskID == "" {
		t.Fatalf("resp = %s", w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "/tasks/"+resp.TaskID {
		t.Fatalf("Location = %q, want /tasks/%s", loc, resp.TaskID)
	}
}

func TestErrorEnvelope(t *testing.T) {
	_, h := newTestRouter(t)
	w := doJSON(h, "POST", "/tasks/register", `{"uid":1}`, map[string]string{"apikey": testServiceKey})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	var resp struct {
		Detail string `json:"detail"`
		Error  struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body = %s", w.Body.String())
	}
	if resp.Detail == "" || resp.Error.Code != "invalid_request" || resp.Error.Message == "" {
		t.Fatalf("envelope incomplete: %+v", resp)
	}
}

func TestTasksListPagination(t *testing.T) {
	_, h := newTestRouter(t)
	hdr := map[string]string{"apikey": testServiceKey, "X-Uid": "7"}
	ids := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		body := fmt.Sprintf(`{"uid":7,"task_type":"http","payload":{},"executor_url":"http://exec:9000","trigger_time":"2030-01-01T00:0%d:00Z"}`, i)
		w := doJSON(h, "POST", "/tasks/register", body, hdr)
		if w.Code != http.StatusCreated {
			t.Fatalf("register %d: status = %d, body = %s", i, w.Code, w.Body.String())
		}
		var resp struct {
			TaskID string `json:"task_id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.TaskID == "" {
			t.Fatalf("register %d resp = %s", i, w.Body.String())
		}
		ids = append(ids, resp.TaskID)
	}

	fetch := func(query string) listResp {
		t.Helper()
		w := doJSON(h, "GET", "/tasks"+query, "", hdr)
		if w.Code != http.StatusOK {
			t.Fatalf("list %q: status = %d, body = %s", query, w.Code, w.Body.String())
		}
		var resp listResp
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("list %q body: %v", query, err)
		}
		return resp
	}

	if r := fetch(""); r.Total != 3 || len(r.Tasks) != 3 {
		t.Fatalf("default page: total = %d, tasks = %d, want 3/3", r.Total, len(r.Tasks))
	}
	if r := fetch("?limit=2"); r.Total != 3 || len(r.Tasks) != 2 || r.Limit != 2 {
		t.Fatalf("limit=2: total = %d, tasks = %d, limit = %d", r.Total, len(r.Tasks), r.Limit)
	}
	if r := fetch("?offset=2"); r.Total != 3 || len(r.Tasks) != 1 {
		t.Fatalf("offset=2: total = %d, tasks = %d, want 3/1", r.Total, len(r.Tasks))
	}

	// status filter: flip one task to completed (conditional update)
	if ok, err := repo.ConditionalUpdate(ids[0], "pending", "completed", nil); err != nil || !ok {
		t.Fatalf("conditional update to completed: ok = %v, err = %v", ok, err)
	}
	if r := fetch("?status=completed"); r.Total != 1 || len(r.Tasks) != 1 || r.Tasks[0].TaskID != ids[0] {
		t.Fatalf("status=completed: total = %d, tasks = %+v, want just %s", r.Total, r.Tasks, ids[0])
	}
	if r := fetch("?status=pending"); r.Total != 2 {
		t.Fatalf("status=pending: total = %d, want 2", r.Total)
	}

	// boundary/invalid params → 400
	for _, q := range []string{"?limit=0", "?limit=201", "?offset=-1", "?status=bogus"} {
		w := doJSON(h, "GET", "/tasks"+q, "", hdr)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("list %q: status = %d, want 400", q, w.Code)
		}
	}
}
