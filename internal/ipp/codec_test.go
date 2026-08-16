package ipp

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"reflect"
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

func TestGetJobsDecodesMultipleJobGroups(t *testing.T) {
	jobGroup := func(id int32, name string, state uint32) Group {
		g := Group{Tag: TagJob}
		g.AddInt("job-id", id)
		g.AddString(TagName, "job-name", name)
		g.AddEnum("job-state", state)
		return g
	}
	operations := baseRequest("")

	t.Run("single job", func(t *testing.T) {
		client := newGetJobsTestClient(t, operations, jobGroup(5, "stable-5", JobStatePending))
		got, err := client.GetJobs(context.Background())
		if err != nil {
			t.Fatalf("GetJobs: %v", err)
		}
		want := []JobInfo{{ID: 5, Name: "stable-5", State: domain.RemotePending}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("jobs = %+v, want %+v", got, want)
		}
	})

	t.Run("multiple jobs", func(t *testing.T) {
		client := newGetJobsTestClient(t, operations,
			jobGroup(5, "stable-5", JobStatePending),
			jobGroup(8, "stable-8", JobStateProcessing),
		)
		got, err := client.GetJobs(context.Background())
		if err != nil {
			t.Fatalf("GetJobs: %v", err)
		}
		want := []JobInfo{
			{ID: 5, Name: "stable-5", State: domain.RemotePending},
			{ID: 8, Name: "stable-8", State: domain.RemoteProcessing},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("jobs = %+v, want %+v", got, want)
		}
	})

	t.Run("duplicate non-job group", func(t *testing.T) {
		client := newGetJobsTestClient(t, operations, operations)
		_, err := client.GetJobs(context.Background())
		if err == nil {
			t.Fatal("expected duplicate-group error")
		}
		pe, ok := err.(*ProtoError)
		if !ok || pe.Kind != ErrKindDuplicateGroup {
			t.Fatalf("error = %v, want %s", err, ErrKindDuplicateGroup)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func newGetJobsTestClient(t *testing.T, groups ...Group) *Client {
	t.Helper()
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		request, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("read request: %v", err)
		}
		if len(request) < 8 {
			t.Fatalf("request length = %d, want at least 8", len(request))
		}
		if operation := binary.BigEndian.Uint16(request[2:4]); operation != OpGetJobs {
			t.Fatalf("operation = 0x%04x, want Get-Jobs", operation)
		}
		response := &Message{
			Version:   Version20,
			Code:      StatusOK,
			RequestID: binary.BigEndian.Uint32(request[4:8]),
			Groups:    groups,
		}
		body := readAll(t, EncodeRequestMustSucceed(t, response, nil))
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/ipp"}},
			Body:       io.NopCloser(bytes.NewReader(body)),
		}, nil
	})}
	return NewClient("printer-1", "http://printer.example/ipp", WithHTTPClient(httpClient))
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
