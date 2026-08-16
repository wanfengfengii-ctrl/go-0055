package ipp

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"pressguard/internal/domain"
)

// ippHandler decodes an IPP request message and returns the response
// status code together with the attribute groups to send back.
type ippHandler func(msg *Message) (status uint16, groups []Group)

// newIPPServer starts a test IPP-over-HTTP endpoint. Each request is
// decoded and passed to h; the status and groups h returns are encoded as
// the IPP response. DecodeResponse is generic over the code field, so it
// decodes a request (operation-id) exactly as it decodes a response.
func newIPPServer(t *testing.T, h ippHandler) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody))
		if err != nil {
			t.Errorf("read request body: %v", err)
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		if len(body) < 8 {
			t.Errorf("ipp request too short: %d bytes", len(body))
			http.Error(w, "short", http.StatusBadRequest)
			return
		}
		rid := binary.BigEndian.Uint32(body[4:8])
		msg, err := DecodeResponse(bytes.NewReader(body), rid)
		if err != nil {
			t.Errorf("decode ipp request: %v", err)
			writeIPPResponse(t, w, rid, StatusClientError, nil)
			return
		}
		status, groups := h(msg)
		writeIPPResponse(t, w, rid, status, groups)
	}))
}

// writeIPPResponse encodes and writes an IPP response message.
func writeIPPResponse(t *testing.T, w http.ResponseWriter, rid uint32, status uint16, groups []Group) {
	t.Helper()
	m := &Message{Version: Version20, Code: status, RequestID: rid, Groups: groups}
	r, err := EncodeRequest(m, nil)
	if err != nil {
		t.Errorf("encode response: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/ipp")
	if _, err := io.Copy(w, r); err != nil {
		t.Errorf("write response: %v", err)
	}
}

// createJobGroups builds the attribute groups for a successful Create-Job
// response carrying the given job-id and job-state.
func createJobGroups(jobID int32, state uint32) []Group {
	ops := Group{Tag: TagOperations}
	ops.AddString(TagCharset, "attributes-charset", "utf-8")
	ops.AddString(TagNaturalLang, "attributes-natural-language", "en")
	job := Group{Tag: TagJob}
	job.AddInt("job-id", jobID)
	job.AddEnum("job-state", state)
	return []Group{ops, job}
}

// locateAttr returns the group tag of the first attribute named name and
// whether such an attribute was present.
func locateAttr(msg *Message, name string) (uint8, bool) {
	for _, g := range msg.Groups {
		for _, a := range g.Attrs {
			if a.Name == name {
				return g.Tag, true
			}
		}
	}
	return 0, false
}

// attrValue returns the value of the first attribute named name.
func attrValue(msg *Message, name string) ([]byte, bool) {
	for _, g := range msg.Groups {
		for _, a := range g.Attrs {
			if a.Name == name {
				return a.Value, true
			}
		}
	}
	return nil, false
}

// TestCreateJobStrictEndpointAcceptsTemplateGroup is the regression test
// for client-error-bad-request from strict printers. job-hold-until is a
// Job Template attribute and MUST be sent in its own job-template-
// attributes (job-attributes-tag, 0x02) group; the operation attributes
// (printer-uri, job-name) MUST remain in the operations-attributes group.
// A strict printer rejects a request that places job-hold-until in the
// operations group with client-error-bad-request, and a tolerant one
// silently drops the hold so the job prints after Send-Document and
// bypasses ReleaseJob.
func TestCreateJobStrictEndpointAcceptsTemplateGroup(t *testing.T) {
	var holdGroup uint8
	var seen bool
	srv := newIPPServer(t, func(msg *Message) (uint16, []Group) {
		holdGroup, seen = locateAttr(msg, "job-hold-until")
		if !seen {
			t.Errorf("Create-Job request missing job-hold-until")
			return StatusClientError, nil
		}
		if holdGroup != TagJob {
			// Strict printer: a template attribute in the operations
			// group is malformed.
			return StatusClientError, nil
		}
		if g, ok := locateAttr(msg, "printer-uri"); !ok || g != TagOperations {
			t.Errorf("printer-uri in group 0x%02x, want operations", g)
		}
		if g, ok := locateAttr(msg, "job-name"); !ok || g != TagOperations {
			t.Errorf("job-name in group 0x%02x, want operations", g)
		}
		if v, ok := attrValue(msg, "job-name"); !ok || string(v) != "pressguard:job1:1" {
			t.Errorf("job-name = %q, want %q", v, "pressguard:job1:1")
		}
		return StatusOK, createJobGroups(42, JobStatePendingHeld)
	})
	defer srv.Close()

	c := NewClient("p1", srv.URL, WithHTTPClient(srv.Client()))
	id, state, err := c.CreateJob(context.Background(), "pressguard:job1:1")
	if err != nil {
		t.Fatalf("CreateJob rejected by strict endpoint: %v", err)
	}
	if !seen {
		t.Fatal("job-hold-until not observed in request")
	}
	if holdGroup != TagJob {
		t.Fatalf("job-hold-until in group 0x%02x, want job-template 0x%02x", holdGroup, TagJob)
	}
	if id != 42 {
		t.Fatalf("job id = %d, want 42", id)
	}
	if state != domain.RemoteHeld {
		t.Fatalf("state = %q, want held", state)
	}
}

// TestCreateJobReturnsPrinterJobID verifies that CreateJob stably returns
// the printer-assigned job id decoded from a successful response, across
// several distinct ids.
func TestCreateJobReturnsPrinterJobID(t *testing.T) {
	for _, wantID := range []int32{1, 999, 1042} {
		srv := newIPPServer(t, func(msg *Message) (uint16, []Group) {
			return StatusOK, createJobGroups(wantID, JobStatePendingHeld)
		})
		c := NewClient("p1", srv.URL, WithHTTPClient(srv.Client()))
		id, _, err := c.CreateJob(context.Background(), "pressguard:job1:1")
		srv.Close()
		if err != nil {
			t.Fatalf("CreateJob(%d): %v", wantID, err)
		}
		if id != int64(wantID) {
			t.Fatalf("CreateJob(%d): id = %d, want %d", wantID, id, wantID)
		}
	}
}

// TestCreateJobReportsHeldState verifies the held semantics: the client
// requests job-hold-until=indefinite in the job-template group and, when
// the printer reflects that the job is held (job-state PendingHeld),
// reports RemoteHeld rather than pending/unknown.
func TestCreateJobReportsHeldState(t *testing.T) {
	var holdVal string
	srv := newIPPServer(t, func(msg *Message) (uint16, []Group) {
		if v, ok := attrValue(msg, "job-hold-until"); ok {
			holdVal = string(v)
		}
		if g, ok := locateAttr(msg, "job-hold-until"); !ok || g != TagJob {
			t.Errorf("job-hold-until in group 0x%02x, want job-template", g)
		}
		return StatusOK, createJobGroups(7, JobStatePendingHeld)
	})
	defer srv.Close()

	c := NewClient("p1", srv.URL, WithHTTPClient(srv.Client()))
	_, state, err := c.CreateJob(context.Background(), "pressguard:job1:2")
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if holdVal != "indefinite" {
		t.Fatalf("job-hold-until = %q, want %q", holdVal, "indefinite")
	}
	if state != domain.RemoteHeld {
		t.Fatalf("state = %q, want held", state)
	}
}
