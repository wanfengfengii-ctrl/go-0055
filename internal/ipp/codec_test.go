package ipp

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"strings"
	"testing"

	"pressguard/internal/domain"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	m := &Message{Version: Version20, Code: OpCreateJob, RequestID: 7}
	ops := baseRequest("alice")
	ops.AddString(TagURI, "printer-uri", "http://h/ipp")
	ops.AddString(TagName, "job-name", "pressguard:job1:1")
	m.Groups = []Group{ops}
	raw := readAll(t, EncodeRequestMustSucceed(t, m, nil))
	dec, err := DecodeResponse(bytes.NewReader(raw), 7)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dec.Code != OpCreateJob {
		t.Fatalf("code = %x", dec.Code)
	}
	if dec.RequestID != 7 {
		t.Fatalf("rid = %d", dec.RequestID)
	}
	a := dec.Find(TagOperations, "job-name")
	if a == nil || string(a.Value) != "pressguard:job1:1" {
		t.Fatalf("job-name missing: %+v", a)
	}
}

func EncodeRequestMustSucceed(t *testing.T, m *Message, data io.Reader) io.Reader {
	t.Helper()
	r, err := EncodeRequest(m, data)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return r
}

func readAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("readall: %v", err)
	}
	return b
}

func TestDecodeRejectsDuplicateScalar(t *testing.T) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, Version20)
	binary.Write(&buf, binary.BigEndian, StatusOK)
	binary.Write(&buf, binary.BigEndian, uint32(1))
	buf.WriteByte(TagJob)
	writeAttr(&buf, TagInteger, "job-id", int32(5))
	writeAttr(&buf, TagInteger, "job-id", int32(6)) // duplicate name
	buf.WriteByte(TagEnd)
	if _, err := DecodeResponse(&buf, 1); err == nil {
		t.Fatal("expected duplicate-scalar error")
	} else if !strings.Contains(err.Error(), "duplicate_scalar") {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestDecodeRejectsIllegalTag(t *testing.T) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, Version20)
	binary.Write(&buf, binary.BigEndian, StatusOK)
	binary.Write(&buf, binary.BigEndian, uint32(1))
	buf.WriteByte(0xFF) // illegal group tag
	buf.WriteByte(TagEnd)
	if _, err := DecodeResponse(&buf, 1); err == nil {
		t.Fatal("expected illegal-tag error")
	}
}

func TestDecodeRejectsTruncated(t *testing.T) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, Version20)
	binary.Write(&buf, binary.BigEndian, StatusOK)
	binary.Write(&buf, binary.BigEndian, uint32(1))
	buf.WriteByte(TagJob)
	writeAttr(&buf, TagInteger, "job-id", int32(5))
	buf.WriteByte(TagName)
	binary.Write(&buf, binary.BigEndian, uint16(7))
	buf.WriteString("job-nam") // short
	if _, err := DecodeResponse(&buf, 1); err == nil {
		t.Fatal("expected truncated error")
	}
}

func TestDecodeRejectsRequestIDMismatch(t *testing.T) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, Version20)
	binary.Write(&buf, binary.BigEndian, StatusOK)
	binary.Write(&buf, binary.BigEndian, uint32(99))
	buf.WriteByte(TagEnd)
	if _, err := DecodeResponse(&buf, 1); err == nil {
		t.Fatal("expected request-id mismatch")
	}
}

func TestDecodeRejectsAttrWithoutGroup(t *testing.T) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, Version20)
	binary.Write(&buf, binary.BigEndian, StatusOK)
	binary.Write(&buf, binary.BigEndian, uint32(1))
	writeAttr(&buf, TagInteger, "job-id", int32(5))
	buf.WriteByte(TagEnd)
	if _, err := DecodeResponse(&buf, 1); err == nil {
		t.Fatal("expected attribute-without-group error")
	}
}

func TestDecodeRejectsDuplicateGroup(t *testing.T) {
	// Only the job-attributes group (TagJob) may legitimately repeat
	// (one per job in a Get-Jobs response). Duplicate printer-,
	// operations-, unsupported- or subscription-attributes groups remain
	// genuine protocol errors.
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, Version20)
	binary.Write(&buf, binary.BigEndian, StatusOK)
	binary.Write(&buf, binary.BigEndian, uint32(1))
	buf.WriteByte(TagPrinter)
	writeAttr(&buf, TagEnum, "printer-state", int32(3))
	buf.WriteByte(TagPrinter) // duplicate printer-attributes group
	buf.WriteByte(TagEnd)
	if _, err := DecodeResponse(&buf, 1); err == nil {
		t.Fatal("expected duplicate-group error")
	} else if !strings.Contains(err.Error(), "duplicate_group") {
		t.Fatalf("wrong error: %v", err)
	}
}

// TestDecodeRejectsDuplicateOperationsGroup checks that a duplicate
// operations-attributes group is still rejected even though job groups
// may now repeat.
func TestDecodeRejectsDuplicateOperationsGroup(t *testing.T) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, Version20)
	binary.Write(&buf, binary.BigEndian, StatusOK)
	binary.Write(&buf, binary.BigEndian, uint32(1))
	buf.WriteByte(TagOperations)
	writeAttr(&buf, TagKeyword, "status-message", int32(0))
	buf.WriteByte(TagOperations) // duplicate operations-attributes group
	buf.WriteByte(TagEnd)
	if _, err := DecodeResponse(&buf, 1); err == nil {
		t.Fatal("expected duplicate-group error")
	} else if !strings.Contains(err.Error(), "duplicate_group") {
		t.Fatalf("wrong error: %v", err)
	}
}

// TestDecodeSingleJobGroup covers a Get-Jobs response with exactly one
// job: one operations group followed by a single job-attributes group.
func TestDecodeSingleJobGroup(t *testing.T) {
	m := &Message{Version: Version20, Code: StatusOK, RequestID: 1}
	ops := Group{Tag: TagOperations}
	ops.AddString(TagCharset, "attributes-charset", "utf-8")
	ops.AddString(TagNaturalLang, "attributes-natural-language", "en")
	job := Group{Tag: TagJob}
	job.AddInt("job-id", 11)
	job.AddString(TagName, "job-name", "pressguard:job1:1")
	job.AddEnum("job-state", JobStatePendingHeld)
	m.Groups = []Group{ops, job}
	raw := readAll(t, EncodeRequestMustSucceed(t, m, nil))

	msg, err := DecodeResponse(bytes.NewReader(raw), 1)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	jobs := collectJobIDs(t, msg)
	if len(jobs) != 1 || jobs[0] != 11 {
		t.Fatalf("expected [11], got %v", jobs)
	}
	if name := findJobName(msg, 0); name != "pressguard:job1:1" {
		t.Fatalf("job-name = %q", name)
	}
	if s := findJobState(msg, 0); s != domain.RemoteHeld {
		t.Fatalf("state = %q", s)
	}
}

// TestDecodeMultipleJobGroups covers the regression: a Get-Jobs success
// response carrying several jobs, each in its own job-attributes group,
// must decode fully instead of failing with duplicate_group.
func TestDecodeMultipleJobGroups(t *testing.T) {
	m := &Message{Version: Version20, Code: StatusOK, RequestID: 1}
	ops := Group{Tag: TagOperations}
	ops.AddString(TagCharset, "attributes-charset", "utf-8")
	ops.AddString(TagNaturalLang, "attributes-natural-language", "en")
	j1 := Group{Tag: TagJob}
	j1.AddInt("job-id", 11)
	j1.AddString(TagName, "job-name", "pressguard:job1:1")
	j1.AddEnum("job-state", JobStatePendingHeld)
	j2 := Group{Tag: TagJob}
	j2.AddInt("job-id", 22)
	j2.AddString(TagName, "job-name", "pressguard:job2:1")
	j2.AddEnum("job-state", JobStateProcessing)
	j3 := Group{Tag: TagJob}
	j3.AddInt("job-id", 33)
	j3.AddString(TagName, "job-name", "pressguard:job3:1")
	j3.AddEnum("job-state", JobStateCompleted)
	m.Groups = []Group{ops, j1, j2, j3}
	raw := readAll(t, EncodeRequestMustSucceed(t, m, nil))

	msg, err := DecodeResponse(bytes.NewReader(raw), 1)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	jobs := collectJobIDs(t, msg)
	if len(jobs) != 3 || jobs[0] != 11 || jobs[1] != 22 || jobs[2] != 33 {
		t.Fatalf("expected [11 22 33], got %v", jobs)
	}
	if name := findJobName(msg, 1); name != "pressguard:job2:1" {
		t.Fatalf("job1 name = %q", name)
	}
	if s := findJobState(msg, 2); s != domain.RemoteCompleted {
		t.Fatalf("job2 state = %q", s)
	}
}

// TestGetJobsSingleJob exercises the single-job Get-Jobs path end to end
// so the multi-job fix does not regress the common one-job case.
func TestGetJobsSingleJob(t *testing.T) {
	httpc := &http.Client{Transport: echoTransport(func(rid uint32) []byte {
		m := &Message{Version: Version20, Code: StatusOK, RequestID: rid}
		ops := Group{Tag: TagOperations}
		ops.AddString(TagCharset, "attributes-charset", "utf-8")
		job := Group{Tag: TagJob}
		job.AddInt("job-id", 7)
		job.AddString(TagName, "job-name", "pressguard:job7:1")
		job.AddEnum("job-state", JobStatePendingHeld)
		m.Groups = []Group{ops, job}
		return readAll(t, EncodeRequestMustSucceed(t, m, nil))
	})}
	c := NewClient("p", "http://h/ipp", WithHTTPClient(httpc))
	jobs, err := c.GetJobs(context.Background())
	if err != nil {
		t.Fatalf("GetJobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != 7 || jobs[0].Name != "pressguard:job7:1" {
		t.Fatalf("jobs = %+v", jobs)
	}
}

// TestGetJobsMultipleJobs exercises the Get-Jobs list boundary end to end:
// a printer returning two jobs in one response must yield both, and
// FindJobByName must recover either job by its stable token for
// resumption after a lost Create-Job response.
func TestGetJobsMultipleJobs(t *testing.T) {
	httpc := &http.Client{Transport: echoTransport(func(rid uint32) []byte {
		m := &Message{Version: Version20, Code: StatusOK, RequestID: rid}
		ops := Group{Tag: TagOperations}
		ops.AddString(TagCharset, "attributes-charset", "utf-8")
		ops.AddString(TagNaturalLang, "attributes-natural-language", "en")
		j1 := Group{Tag: TagJob}
		j1.AddInt("job-id", 11)
		j1.AddString(TagName, "job-name", "pressguard:job1:1")
		j1.AddEnum("job-state", JobStatePendingHeld)
		j2 := Group{Tag: TagJob}
		j2.AddInt("job-id", 22)
		j2.AddString(TagName, "job-name", "pressguard:job2:1")
		j2.AddEnum("job-state", JobStateProcessing)
		m.Groups = []Group{ops, j1, j2}
		return readAll(t, EncodeRequestMustSucceed(t, m, nil))
	})}
	c := NewClient("p", "http://h/ipp", WithHTTPClient(httpc))
	jobs, err := c.GetJobs(context.Background())
	if err != nil {
		t.Fatalf("GetJobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d: %+v", len(jobs), jobs)
	}
	if jobs[0].ID != 11 || jobs[0].Name != "pressguard:job1:1" || jobs[0].State != domain.RemoteHeld {
		t.Fatalf("job0 = %+v", jobs[0])
	}
	if jobs[1].ID != 22 || jobs[1].Name != "pressguard:job2:1" || jobs[1].State != domain.RemoteProcessing {
		t.Fatalf("job1 = %+v", jobs[1])
	}

	// FindJobByName issues a fresh Get-Jobs; the transport echoes the
	// request id so recovery by stable job-name succeeds.
	if ji, ok, err := c.FindJobByName(context.Background(), "pressguard:job2:1"); err != nil || !ok || ji.ID != 22 {
		t.Fatalf("FindJobByName = %+v ok=%v err=%v", ji, ok, err)
	}
	if _, ok, err := c.FindJobByName(context.Background(), "no-such-token"); err != nil || ok {
		t.Fatalf("FindJobByName(missing) ok=%v err=%v", ok, err)
	}
}

type roundTripFunc func(*http.Request) *http.Response

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req), nil
}

// echoTransport returns a transport that serves build(rid) for every
// request, extracting the request id from the IPP request header so the
// response matches whatever id the client sent (across multiple calls).
func echoTransport(build func(rid uint32) []byte) http.RoundTripper {
	return roundTripFunc(func(req *http.Request) *http.Response {
		rid := requestIDFromRequest(req)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/ipp"}},
			Body:       io.NopCloser(bytes.NewReader(build(rid))),
		}
	})
}

// requestIDFromRequest reads the IPP request body and returns the
// request-id (header bytes 4..8).
func requestIDFromRequest(req *http.Request) uint32 {
	body, err := io.ReadAll(req.Body)
	if err != nil || len(body) < 8 {
		return 0
	}
	return binary.BigEndian.Uint32(body[4:8])
}

func writeAttr(buf *bytes.Buffer, tag uint8, name string, val int32) {
	buf.WriteByte(tag)
	binary.Write(buf, binary.BigEndian, uint16(len(name)))
	buf.WriteString(name)
	var vb [4]byte
	binary.BigEndian.PutUint32(vb[:], uint32(val))
	binary.Write(buf, binary.BigEndian, uint16(4))
	buf.Write(vb[:])
}

// collectJobIDs returns the job-id of every job-attributes group in msg,
// in order, mirroring the invariant GetJobs relies on.
func collectJobIDs(t *testing.T, msg *Message) []int64 {
	t.Helper()
	var ids []int64
	for _, g := range msg.Groups {
		if g.Tag != TagJob {
			continue
		}
		if a := findAttr(g.Attrs, "job-id"); a != nil {
			if v, ok := AsInt(a.Value); ok {
				ids = append(ids, int64(v))
			}
		}
	}
	return ids
}

// findJobName returns the job-name of the jobIndex-th job-attributes group.
func findJobName(msg *Message, jobIndex int) string {
	i := 0
	for _, g := range msg.Groups {
		if g.Tag != TagJob {
			continue
		}
		if i == jobIndex {
			if a := findAttr(g.Attrs, "job-name"); a != nil {
				return string(a.Value)
			}
		}
		i++
	}
	return ""
}

// findJobState returns the mapped state of the jobIndex-th job-attributes
// group.
func findJobState(msg *Message, jobIndex int) domain.RemoteState {
	i := 0
	for _, g := range msg.Groups {
		if g.Tag != TagJob {
			continue
		}
		if i == jobIndex {
			if a := findAttr(g.Attrs, "job-state"); a != nil {
				return mapJobState(a.Value)
			}
		}
		i++
	}
	return domain.RemoteUnknown
}
