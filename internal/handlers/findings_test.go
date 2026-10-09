package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stuttgart-things/homerun2-schedule-pitcher/internal/findings"
)

type fakeFindings struct {
	got   findings.Report
	acked string
	err   error
}

func (f *fakeFindings) Ingest(_ context.Context, r findings.Report) (findings.IngestResult, error) {
	f.got = r
	return findings.IngestResult{Source: r.Source, Open: len(r.Findings)}, f.err
}

func (f *fakeFindings) Acknowledge(_ context.Context, source, key, by, note string) (findings.Finding, error) {
	f.acked = source + "|" + key + "|" + by + "|" + note
	return findings.Finding{Source: source, Key: key, Status: findings.StatusAcknowledged}, f.err
}

func (f *fakeFindings) List(_ context.Context, status, source string) ([]findings.Finding, error) {
	return nil, f.err
}

func (f *fakeFindings) Reminders(context.Context) ([]findings.ReminderStatus, error) {
	return []findings.ReminderStatus{{Reminder: findings.Reminder{ID: "r", Title: "t"}, Status: findings.ReminderOpen}}, f.err
}

func (f *fakeFindings) ReminderDone(_ context.Context, id, by string) (findings.ReminderStatus, error) {
	if id != "r" {
		return findings.ReminderStatus{}, findings.ErrUnknownReminder
	}
	f.acked = id + "|" + by
	return findings.ReminderStatus{Reminder: findings.Reminder{ID: id}, Status: findings.ReminderDone}, f.err
}

func doDone(f *fakeFindings, id, body string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/reminders/{id}/done", NewReminderDoneHandler(f))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/reminders/"+id+"/done", strings.NewReader(body)))
	return rr
}

func TestReminderHandlers(t *testing.T) {
	f := &fakeFindings{}
	rr := do(NewRemindersHandler(f), http.MethodGet, "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"status":"open"`) {
		t.Fatalf("list: %d %s", rr.Code, rr.Body)
	}
	if rr := doDone(f, "r", ""); rr.Code != http.StatusOK || f.acked != "r|api" {
		t.Fatalf("done without body: %d %q", rr.Code, f.acked)
	}
	if rr := doDone(f, "r", `{"by":"patrick"}`); rr.Code != http.StatusOK || f.acked != "r|patrick" || !strings.Contains(rr.Body.String(), `"status":"done"`) {
		t.Fatalf("done: %d %q %s", rr.Code, f.acked, rr.Body)
	}
	if rr := doDone(f, "nope", ""); rr.Code != http.StatusNotFound {
		t.Errorf("unknown: %d", rr.Code)
	}
	if rr := doDone(f, "r", `{`); rr.Code != http.StatusBadRequest {
		t.Errorf("bad JSON: %d", rr.Code)
	}
}

func do(h http.HandlerFunc, method, body string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(method, "/", strings.NewReader(body)))
	return rr
}

func TestIngestHandler(t *testing.T) {
	f := &fakeFindings{}
	h := NewIngestHandler(f)
	rr := do(h, http.MethodPost, `{"source":"dev-maintenance","run":"r1","findings":[
		{"key":"dev4-vm/disk:/var","title":"/var at 74%","severity":"Warning","value":74,"threshold":70,"host":"dev4-vm"}]}`)
	if rr.Code != http.StatusOK || f.got.Findings[0].Severity != "warning" || *f.got.Findings[0].Value != 74 {
		t.Fatalf("code %d body %s report %+v", rr.Code, rr.Body, f.got)
	}
	var res findings.IngestResult
	_ = json.NewDecoder(rr.Body).Decode(&res)
	if res.Open != 1 {
		t.Fatalf("result = %+v", res)
	}

	for body, want := range map[string]int{
		`{"source":"x","findings":[{"key":"k","title":"t","severity":"loud"}]}`: http.StatusBadRequest,
		`{"source":"x","findngs":[]}`:                                           http.StatusBadRequest,
		`not json`:                                                              http.StatusBadRequest,
		`{"source":"x","findings":[]}`:                                          http.StatusOK,
		`{"source":"x","findings":[{"key":"k","title":"t","severity":"info","message":"` + strings.Repeat("a", findings.MaxBodyBytes) + `"}]}`: http.StatusRequestEntityTooLarge,
	} {
		if rr := do(h, http.MethodPost, body); rr.Code != want {
			t.Errorf("body %.40q: code %d, want %d (%s)", body, rr.Code, want, rr.Body)
		}
	}
}

func TestAckHandler(t *testing.T) {
	f := &fakeFindings{}
	h := NewAckHandler(f)
	rr := do(h, http.MethodPost, `{"source":"dev","key":"vm/disk:/home","by":"patrick","note":"cleanup"}`)
	if rr.Code != http.StatusOK || f.acked != "dev|vm/disk:/home|patrick|cleanup" {
		t.Fatalf("code %d acked %q", rr.Code, f.acked)
	}
	if rr := do(h, http.MethodPost, `{"source":"dev","key":"k"}`); rr.Code != http.StatusBadRequest {
		t.Errorf("missing by: %d", rr.Code)
	}
	f.err = findings.ErrNotFound
	if rr := do(h, http.MethodPost, `{"source":"dev","key":"k","by":"p"}`); rr.Code != http.StatusNotFound {
		t.Errorf("not found: %d", rr.Code)
	}
}

func TestFindingsHandler(t *testing.T) {
	h := NewFindingsHandler(&fakeFindings{})
	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodGet, "/api/findings?status=open", nil))
	if rr.Code != http.StatusOK || strings.TrimSpace(rr.Body.String()) != "[]" {
		t.Fatalf("code %d body %s", rr.Code, rr.Body)
	}
	rr = httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodGet, "/api/findings?status=weird", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad status: %d", rr.Code)
	}
}
