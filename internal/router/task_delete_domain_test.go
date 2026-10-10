package router

// Tests for DELETE /tasks/:task_id (owner-scoped, in-flight guarded) and the
// domain namespace: register validation, list scoping (?domain=) and
// lowercase normalization.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"app-task/internal/repo"
	"app-task/internal/service"
)

func TestTasksDeletePending(t *testing.T) {
	_, h := newTestRouter(t)
	regBody := `{"uid":1,"task_type":"http","payload":{"k":"v"},"executor_url":"http://exec:9000","trigger_time":"2030-01-01T00:00:00Z"}`
	w := doJSON(h, "POST", "/tasks/register", regBody, map[string]string{"apikey": testServiceKey})
	if w.Code != http.StatusCreated {
		t.Fatalf("register: status = %d, body = %s", w.Code, w.Body.String())
	}
	var reg struct {
		TaskID string `json:"task_id"`
	}
	json.Unmarshal(w.Body.Bytes(), &reg)

	// foreign uid cannot delete → 403
	if w := doJSON(h, "DELETE", "/tasks/"+reg.TaskID, "", map[string]string{"apikey": testServiceKey, "X-Uid": "2"}); w.Code != http.StatusForbidden {
		t.Fatalf("foreign delete: status = %d, body = %s", w.Code, w.Body.String())
	}

	// owner deletes the pending task → 200 {deleted:true}
	w = doJSON(h, "DELETE", "/tasks/"+reg.TaskID, "", map[string]string{"apikey": testServiceKey, "X-Uid": "1"})
	if w.Code != http.StatusOK {
		t.Fatalf("delete: status = %d, body = %s", w.Code, w.Body.String())
	}
	var del struct {
		TaskID  string `json:"task_id"`
		Deleted bool   `json:"deleted"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &del); err != nil || !del.Deleted || del.TaskID != reg.TaskID {
		t.Fatalf("delete body = %s", w.Body.String())
	}

	// row is gone; detail answers 404 and a second delete answers 404
	if task, _ := repo.GetTaskByID(reg.TaskID); task != nil {
		t.Fatalf("task row still present after delete")
	}
	if w := doJSON(h, "GET", "/tasks/"+reg.TaskID, "", map[string]string{"apikey": testServiceKey, "X-Uid": "1"}); w.Code != http.StatusNotFound {
		t.Fatalf("detail after delete: status = %d, want 404", w.Code)
	}
	if w := doJSON(h, "DELETE", "/tasks/"+reg.TaskID, "", map[string]string{"apikey": testServiceKey, "X-Uid": "1"}); w.Code != http.StatusNotFound {
		t.Fatalf("second delete: status = %d, want 404", w.Code)
	}
}

func TestTasksDeleteInFlightRefused(t *testing.T) {
	svc, h := newTestRouter(t)
	taskID, err := svc.RegisterTask(service.RegisterOptions{UID: 1, TaskType: "http", Payload: []byte(`{}`), ExecutorURL: "http://exec", TriggerTime: time.Now().UTC().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := repo.ConditionalUpdate(taskID, "pending", "running", nil); !ok {
		t.Fatalf("claim failed")
	}
	w := doJSON(h, "DELETE", "/tasks/"+taskID, "", map[string]string{"apikey": testServiceKey, "X-Uid": "1"})
	if w.Code != http.StatusConflict {
		t.Fatalf("delete running: status = %d, want 409, body = %s", w.Code, w.Body.String())
	}
	if code := errCode(t, w); code != "task_in_flight" {
		t.Fatalf("code = %q, want task_in_flight", code)
	}
	// terminal tasks stay deletable
	repo.ConditionalUpdate(taskID, "running", "completed", nil)
	if w := doJSON(h, "DELETE", "/tasks/"+taskID, "", map[string]string{"apikey": testServiceKey, "X-Uid": "1"}); w.Code != http.StatusOK {
		t.Fatalf("delete completed: status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestTasksDeleteMissing(t *testing.T) {
	_, h := newTestRouter(t)
	w := doJSON(h, "DELETE", "/tasks/no-such-task", "", map[string]string{"apikey": testServiceKey, "X-Uid": "1"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// uid may arrive as a JSON string (the main app sends uids as strings because
// they exceed JS's 2^53 safe integer) or as a number — both decode exactly.
func TestTasksRegisterStringUID(t *testing.T) {
	_, h := newTestRouter(t)
	body := `{"uid":"7","task_type":"http","payload":{},"executor_url":"http://exec:9000","trigger_time":"2030-01-01T00:00:00Z"}`
	w := doJSON(h, "POST", "/tasks/register", body, map[string]string{"apikey": testServiceKey})
	if w.Code != http.StatusCreated {
		t.Fatalf("string uid register: status = %d, body = %s", w.Code, w.Body.String())
	}
	var reg struct {
		TaskID string `json:"task_id"`
	}
	json.Unmarshal(w.Body.Bytes(), &reg)
	task, _ := repo.GetTaskByID(reg.TaskID)
	if task == nil || task.UID != 7 {
		t.Fatalf("task = %+v, want uid 7", task)
	}
	// the string uid owns detail/delete like any other task
	if w := doJSON(h, "GET", "/tasks/"+reg.TaskID, "", map[string]string{"apikey": testServiceKey, "X-Uid": "7"}); w.Code != http.StatusOK {
		t.Fatalf("detail as string uid: status = %d", w.Code)
	}
	if w := doJSON(h, "DELETE", "/tasks/"+reg.TaskID, "", map[string]string{"apikey": testServiceKey, "X-Uid": "7"}); w.Code != http.StatusOK {
		t.Fatalf("delete as string uid: status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestTasksRegisterDomainValidation(t *testing.T) {
	_, h := newTestRouter(t)
	hdr := map[string]string{"apikey": testServiceKey}

	// invalid charset → 400
	bad := `{"uid":1,"domain":"Bad Domain!","trigger_time":"2030-01-01T00:00:00Z"}`
	if w := doJSON(h, "POST", "/tasks/register", bad, hdr); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid domain: status = %d, body = %s", w.Code, w.Body.String())
	}

	// valid domain (upper-case input normalized to lowercase) → 201
	good := `{"uid":1,"domain":"Task_Quiz","trigger_time":"2030-01-01T00:00:00Z"}`
	w := doJSON(h, "POST", "/tasks/register", good, hdr)
	if w.Code != http.StatusCreated {
		t.Fatalf("valid domain: status = %d, body = %s", w.Code, w.Body.String())
	}
	var reg struct {
		TaskID string `json:"task_id"`
	}
	json.Unmarshal(w.Body.Bytes(), &reg)
	task, _ := repo.GetTaskByID(reg.TaskID)
	if task == nil || task.Domain != "task_quiz" {
		t.Fatalf("task = %+v, want domain=task_quiz", task)
	}
}

func TestTasksListDomainFilter(t *testing.T) {
	svc, h := newTestRouter(t)
	hdr := map[string]string{"apikey": testServiceKey, "X-Uid": "1"}
	for _, d := range []string{"task_quiz", "pay", ""} {
		if _, err := svc.RegisterTask(service.RegisterOptions{UID: 1, TaskType: "http", Payload: []byte(`{}`), ExecutorURL: "http://exec", TriggerTime: time.Now().UTC().Add(time.Minute), Domain: d}); err != nil {
			t.Fatal(err)
		}
	}
	// unfiltered list returns all 3
	if w := doJSON(h, "GET", "/tasks", "", hdr); w.Code != http.StatusOK {
		t.Fatalf("list: status = %d", w.Code)
	} else {
		var resp listResp
		json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.Total != 3 {
			t.Fatalf("unfiltered total = %d, want 3", resp.Total)
		}
	}
	// domain filter narrows to the matching rows
	for _, domain := range []string{"task_quiz", "pay"} {
		w := doJSON(h, "GET", fmt.Sprintf("/tasks?domain=%s", domain), "", hdr)
		if w.Code != http.StatusOK {
			t.Fatalf("list domain=%s: status = %d", domain, w.Code)
		}
		var resp listResp
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("body: %v", err)
		}
		if resp.Total != 1 || len(resp.Tasks) != 1 {
			t.Fatalf("domain=%s: total = %d, tasks = %d, want 1/1", domain, resp.Total, len(resp.Tasks))
		}
	}
	// invalid domain → 400
	if w := doJSON(h, "GET", "/tasks?domain=bad%20domain", "", hdr); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid domain filter: status = %d, want 400", w.Code)
	}
}
