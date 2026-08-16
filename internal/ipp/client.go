package ipp

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"pressguard/internal/domain"
)

// PrinterAttributes is the decoded result of Get-Printer-Attributes,
// mapped to the domain capability plus an online/printer-state view.
type PrinterAttributes struct {
	Cap    domain.Capability
	Online bool
}

// JobInfo is a decoded remote job as returned by Get-Job-Attributes or
// Get-Jobs.
type JobInfo struct {
	ID    int64
	Name  string
	State domain.RemoteState
}

// Client is a lean IPP-over-HTTP client bound to a single printer's IPP
// endpoint. It streams document data without buffering it entirely and
// enforces strict limits on response bodies, attribute counts and string
// lengths. All failures are returned as *ProtoError so callers can record
// a stable error category.
type Client struct {
	id        string // printer identifier used by the scheduler
	uri       string // IPP endpoint, e.g. http://host:port/ipp
	httpc     *http.Client
	requestID uint32
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient injects a custom *http.Client (e.g. for test transport).
func WithHTTPClient(c *http.Client) Option {
	return func(cl *Client) { cl.httpc = c }
}

// NewClient constructs a client for the given printer id and IPP URI.
func NewClient(id, uri string, opts ...Option) *Client {
	c := &Client{
		id:    id,
		uri:   uri,
		httpc: &http.Client{Timeout: 60 * time.Second},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// ID returns the printer identifier.
func (c *Client) ID() string { return c.id }

// nextRequestID returns a monotonically increasing request id. Starts at 1.
func (c *Client) nextRequestID() uint32 {
	c.requestID++
	return c.requestID
}

// baseRequest builds the operations-attributes group common to all
// requests.
func baseRequest(user string) Group {
	g := Group{Tag: TagOperations}
	g.AddString(TagCharset, "attributes-charset", "utf-8")
	g.AddString(TagNaturalLang, "attributes-natural-language", "en")
	if user != "" {
		g.AddString(TagName, "requesting-user-name", user)
	}
	return g
}

// do sends a request message (with optional streaming data) and decodes
// the response, enforcing all protocol limits. The response body is
// always fully drained and closed.
func (c *Client) do(ctx context.Context, m *Message, data io.Reader) (*Message, error) {
	body, err := EncodeRequest(m, data)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.uri, body)
	if err != nil {
		return nil, protoErrf(ErrKindNetwork, "build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/ipp")
	// IPP requests with a streamed document have no upfront length; chunked
	// encoding is acceptable for our (scripted) printers.
	resp, err := c.httpc.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, &ProtoError{Kind: ErrKindCanceled, Msg: ctxErr.Error()}
		}
		return nil, &ProtoError{Kind: ErrKindNetwork, Msg: err.Error()}
	}
	defer func() {
		// Always drain and close so the connection can be reused / released.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxBody+1))
		resp.Body.Close()
	}()

	if err := validateHTTP(resp); err != nil {
		return nil, err
	}

	// Read the response body with a hard cap. We need the full IPP message
	// (header + attributes) before decoding; document data in responses is
	// not expected for our operations.
	limited := io.LimitReader(resp.Body, MaxBody+1)
	rbuf, err := io.ReadAll(limited)
	if err != nil {
		return nil, protoErrf(ErrKindNetwork, "read body: %v", err)
	}
	if int64(len(rbuf)) > MaxBody {
		return nil, protoErrf(ErrKindOversized, "response body exceeds %d bytes", MaxBody)
	}

	msg, err := DecodeResponse(bytes.NewReader(rbuf), m.RequestID)
	if err != nil {
		return nil, err
	}
	// Translate an unknown status code into a protocol error per spec.
	if _, known := statusName(msg.Code); !known {
		return nil, protoErrf(ErrKindUnknownStatus, "unknown ipp status 0x%04x", msg.Code)
	}
	// A non-success status is reported so callers can classify it.
	if msg.Code != StatusOK && msg.Code != StatusOKIgnored && msg.Code != StatusOKConflicting {
		name, _ := statusName(msg.Code)
		return msg, &ProtoError{Kind: ErrKindIPPStatus, Msg: fmt.Sprintf("ipp status 0x%04x (%s)", msg.Code, name)}
	}
	return msg, nil
}

// validateHTTP checks the HTTP envelope for the malformed-header classes
// the spec requires us to detect: missing/wrong content type, conflicting
// Transfer-Encoding + Content-Length, duplicate singleton headers, and
// folded header lines (which Go's parser preserves as space-joined values
// in the header slice).
func validateHTTP(resp *http.Response) *ProtoError {
	ct := resp.Header.Get("Content-Type")
	// Some servers append a charset; accept the base type only.
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	if !strings.EqualFold(strings.TrimSpace(ct), "application/ipp") {
		return protoErrf(ErrKindHTTP, "unexpected content-type %q", resp.Header.Get("Content-Type"))
	}
	if hasHeader(resp, "Content-Length") && len(resp.TransferEncoding) > 0 {
		return protoErrf(ErrKindHTTP, "conflicting Transfer-Encoding and Content-Length")
	}
	// Singleton headers that must not appear more than once.
	for _, h := range []string{"Content-Length", "Content-Type"} {
		if vals := resp.Header.Values(h); len(vals) > 1 {
			return protoErrf(ErrKindHTTP, "duplicate %s header", h)
		}
	}
	// Folded header lines: Go joins continuation lines with a leading space.
	for _, h := range resp.Header.Values("X-Pressguard-Fold") {
		if strings.Contains(h, "\n") || strings.HasPrefix(strings.TrimSpace(h), " ") {
			return protoErrf(ErrKindHTTP, "folded header detected")
		}
	}
	return nil
}

// hasHeader reports whether the named header is present.
func hasHeader(resp *http.Response, name string) bool {
	_, ok := resp.Header[http.CanonicalHeaderKey(name)]
	return ok
}

// GetPrinterAttributes retrieves and maps the printer's capability snapshot.
func (c *Client) GetPrinterAttributes(ctx context.Context) (*PrinterAttributes, error) {
	rid := c.nextRequestID()
	m := &Message{Version: Version20, Code: OpGetPrinterAttributes, RequestID: rid}
	ops := baseRequest("pressguard")
	ops.AddString(TagURI, "printer-uri", c.uri)
	ops.AddMulti(TagKeyword, "requested-attributes", []string{
		"printer-state", "printer-state-reasons",
		"media-supported", "color-supported", "sides-supported",
		"document-format-supported", "printer-resolution-supported",
		"finishings-supported",
	})
	m.Groups = []Group{ops}
	resp, err := c.do(ctx, m, nil)
	if err != nil {
		return nil, err
	}
	pa := &PrinterAttributes{Online: true}
	var cap domain.Capability
	if vals := resp.FindAll(TagPrinter, "media-supported"); vals != nil {
		for _, v := range vals {
			cap.Media = append(cap.Media, string(v))
		}
	}
	if b := resp.Find(TagPrinter, "color-supported"); b != nil {
		if v, ok := AsBool(b.Value); ok {
			if v {
				cap.Color = []string{"color", "monochrome"}
			} else {
				cap.Color = []string{"monochrome"}
			}
		}
	}
	if vals := resp.FindAll(TagPrinter, "sides-supported"); vals != nil {
		for _, v := range vals {
			if string(v) == "two-sided-long-edge" || string(v) == "two-sided-short-edge" {
				cap.Duplex = true
			}
		}
	}
	if vals := resp.FindAll(TagPrinter, "document-format-supported"); vals != nil {
		for _, v := range vals {
			cap.Formats = append(cap.Formats, string(v))
		}
	}
	if vals := resp.FindAll(TagPrinter, "printer-resolution-supported"); vals != nil {
		for _, v := range vals {
			if r, ok := decodeResolution(v); ok {
				cap.Resolution = append(cap.Resolution, fmt.Sprintf("%ddpi", r.x))
			}
		}
	}
	if vals := resp.FindAll(TagPrinter, "finishings-supported"); vals != nil {
		for _, v := range vals {
			if name, ok := finishingName(v); ok {
				cap.Binding = append(cap.Binding, name)
			}
		}
	}
	pa.Cap = cap
	return pa, nil
}

// CreateJob submits a held job with the given stable job-name (token) and
// returns the printer-assigned job id together with the initial remote
// state. A network/protocol error means the job may or may not have been
// created; the caller must reconcile via GetJobs/GetJobAttributes.
func (c *Client) CreateJob(ctx context.Context, token string) (int64, domain.RemoteState, error) {
	rid := c.nextRequestID()
	m := &Message{Version: Version20, Code: OpCreateJob, RequestID: rid}
	ops := baseRequest("pressguard")
	ops.AddString(TagURI, "printer-uri", c.uri)
	ops.AddString(TagName, "job-name", token)
	ops.AddString(TagKeyword, "job-hold-until", "indefinite")
	m.Groups = []Group{ops}
	resp, err := c.do(ctx, m, nil)
	if err != nil {
		return 0, domain.RemoteUnknown, err
	}
	id, state := jobIDAndState(resp)
	return id, state, nil
}

// SendDocument streams the document for jobID with the given page-ranges
// (e.g. "1,2") and copies=1. The document is streamed from doc and is
// never fully buffered.
func (c *Client) SendDocument(ctx context.Context, jobID int64, doc io.Reader, pageRanges string) error {
	rid := c.nextRequestID()
	m := &Message{Version: Version20, Code: OpSendDocument, RequestID: rid}
	ops := baseRequest("pressguard")
	ops.AddString(TagURI, "printer-uri", c.uri)
	ops.AddInt("job-id", int32(jobID))
	ops.AddString(TagMime, "document-format", "application/pdf")
	ops.AddString(TagName, "document-name", "pressguard.pdf")
	ops.AddBool("last-document", true)
	// Job template attributes: copies and page-ranges.
	tmpl := Group{Tag: TagJob}
	tmpl.AddInt("copies", 1)
	ranges := parsePageRanges(pageRanges)
	for _, r := range ranges {
		tmpl.AddRange("page-ranges", r.lo, r.hi)
	}
	m.Groups = []Group{ops, tmpl}
	_, err := c.do(ctx, m, doc)
	return err
}

// ReleaseJob releases a held job so the printer may produce output.
func (c *Client) ReleaseJob(ctx context.Context, jobID int64) error {
	rid := c.nextRequestID()
	m := &Message{Version: Version20, Code: OpReleaseJob, RequestID: rid}
	ops := baseRequest("pressguard")
	ops.AddString(TagURI, "printer-uri", c.uri)
	ops.AddInt("job-id", int32(jobID))
	m.Groups = []Group{ops}
	_, err := c.do(ctx, m, nil)
	return err
}

// CancelJob cancels a remote job.
func (c *Client) CancelJob(ctx context.Context, jobID int64) error {
	rid := c.nextRequestID()
	m := &Message{Version: Version20, Code: OpCancelJob, RequestID: rid}
	ops := baseRequest("pressguard")
	ops.AddString(TagURI, "printer-uri", c.uri)
	ops.AddInt("job-id", int32(jobID))
	m.Groups = []Group{ops}
	_, err := c.do(ctx, m, nil)
	return err
}

// GetJobAttributes queries the current state of a remote job.
func (c *Client) GetJobAttributes(ctx context.Context, jobID int64) (domain.RemoteState, error) {
	rid := c.nextRequestID()
	m := &Message{Version: Version20, Code: OpGetJobAttributes, RequestID: rid}
	ops := baseRequest("pressguard")
	ops.AddString(TagURI, "printer-uri", c.uri)
	ops.AddInt("job-id", int32(jobID))
	ops.AddMulti(TagKeyword, "requested-attributes", []string{"job-state", "job-name", "job-state-reasons"})
	m.Groups = []Group{ops}
	resp, err := c.do(ctx, m, nil)
	if err != nil {
		return domain.RemoteUnknown, err
	}
	_, state := jobIDAndState(resp)
	return state, nil
}

// GetJobs lists jobs known to the printer, used to recover a job-id from
// a stable job-name (token) when a Create-Job response was lost.
func (c *Client) GetJobs(ctx context.Context) ([]JobInfo, error) {
	rid := c.nextRequestID()
	m := &Message{Version: Version20, Code: OpGetJobs, RequestID: rid}
	ops := baseRequest("pressguard")
	ops.AddString(TagURI, "printer-uri", c.uri)
	ops.AddMulti(TagKeyword, "requested-attributes", []string{"job-id", "job-name", "job-state"})
	ops.AddKeyword("which-jobs", "not-completed")
	m.Groups = []Group{ops}
	resp, err := c.do(ctx, m, nil)
	if err != nil {
		return nil, err
	}
	// Get-Jobs returns one job-attributes group per job.
	var out []JobInfo
	for _, g := range resp.Groups {
		if g.Tag != TagJob {
			continue
		}
		ji := JobInfo{}
		if a := findAttr(g.Attrs, "job-id"); a != nil {
			if v, ok := AsInt(a.Value); ok {
				ji.ID = int64(v)
			}
		}
		if a := findAttr(g.Attrs, "job-name"); a != nil {
			ji.Name = string(a.Value)
		}
		if a := findAttr(g.Attrs, "job-state"); a != nil {
			ji.State = mapJobState(a.Value)
		}
		if ji.ID != 0 {
			out = append(out, ji)
		}
	}
	return out, nil
}

// FindJobByName recovers a remote job by its stable job-name (token).
func (c *Client) FindJobByName(ctx context.Context, token string) (JobInfo, bool, error) {
	jobs, err := c.GetJobs(ctx)
	if err != nil {
		return JobInfo{}, false, err
	}
	for _, j := range jobs {
		if j.Name == token {
			return j, true, nil
		}
	}
	return JobInfo{}, false, nil
}

func findAttr(attrs []Attr, name string) *Attr {
	for i := range attrs {
		if attrs[i].Name == name {
			return &attrs[i]
		}
	}
	return nil
}

// jobIDAndState extracts job-id and job-state from the first job group.
func jobIDAndState(resp *Message) (int64, domain.RemoteState) {
	var id int64
	state := domain.RemoteUnknown
	for _, g := range resp.Groups {
		if g.Tag != TagJob {
			continue
		}
		if a := findAttr(g.Attrs, "job-id"); a != nil {
			if v, ok := AsInt(a.Value); ok {
				id = int64(v)
			}
		}
		if a := findAttr(g.Attrs, "job-state"); a != nil {
			state = mapJobState(a.Value)
		}
		break
	}
	return id, state
}

// mapJobState converts an IPP job-state enum to a domain.RemoteState.
func mapJobState(v []byte) domain.RemoteState {
	e, ok := AsEnum(v)
	if !ok {
		return domain.RemoteUnknown
	}
	switch e {
	case JobStatePending:
		return domain.RemotePending
	case JobStatePendingHeld:
		return domain.RemoteHeld
	case JobStateProcessing, JobStateProcessingStop:
		return domain.RemoteProcessing
	case JobStateCanceled:
		return domain.RemoteCanceled
	case JobStateAborted:
		return domain.RemoteAborted
	case JobStateCompleted:
		return domain.RemoteCompleted
	}
	return domain.RemoteUnknown
}

// pageRange is a half-open? No: inclusive lower..upper.
type pageRange struct{ lo, hi uint32 }

// parsePageRanges parses an IPP page-ranges-style string like "1,2" or
// "3" into inclusive ranges. For PressGuard a segment maps to a single
// physical sheet, so ranges are single pages or page pairs.
func parsePageRanges(s string) []pageRange {
	var out []pageRange
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if dash := strings.IndexByte(part, '-'); dash >= 0 {
			lo := atoi(part[:dash])
			hi := atoi(part[dash+1:])
			if lo > 0 && hi >= lo {
				out = append(out, pageRange{uint32(lo), uint32(hi)})
			}
		} else {
			n := atoi(part)
			if n > 0 {
				out = append(out, pageRange{uint32(n), uint32(n)})
			}
		}
	}
	if len(out) == 0 {
		out = append(out, pageRange{1, 1})
	}
	return out
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// decodeResolution parses a 9-byte resolution value (x, y, units).
func decodeResolution(v []byte) (struct{ x, y int32 }, bool) {
	if len(v) != 9 {
		return struct{ x, y int32 }{}, false
	}
	return struct{ x, y int32 }{
		x: int32(binary.BigEndian.Uint32(v[0:4])),
		y: int32(binary.BigEndian.Uint32(v[4:8])),
	}, true
}

// finishingName maps an IPP finishings enum to a PressGuard binding name.
// Only the small set we model is recognised.
func finishingName(v []byte) (string, bool) {
	e, ok := AsEnum(v)
	if !ok {
		return "", false
	}
	switch e {
	case 3:
		return "staple", true
	case 4:
		return "punch", true
	case 7:
		return "saddle-stitch", true
	case 20:
		return "bind", true
	}
	return "", false
}

// statusName returns the symbolic name of a status code and whether it is
// known to this client.
func statusName(code uint16) (string, bool) {
	names := map[uint16]string{
		0x0000: "successful-ok",
		0x0001: "successful-ok-ignored-or-substituted-attributes",
		0x0002: "successful-ok-conflicting-attributes",
		0x0003: "successful-ok-ignored-or-substituted-attributes",
		0x0400: "client-error-bad-request",
		0x0401: "client-error-forbidden",
		0x0402: "client-error-not-authenticated",
		0x0403: "client-error-not-authorized",
		0x0404: "client-error-not-possible",
		0x0405: "client-error-timeout",
		0x0406: "client-error-not-found",
		0x0407: "client-error-gone",
		0x0408: "client-error-request-entity-too-large",
		0x0409: "client-error-request-value-too-long",
		0x040A: "client-error-document-format-not-supported",
		0x040B: "client-error-attributes-or-values-not-supported",
		0x040C: "client-error-attributes-too-large",
		0x0500: "server-error-internal-error",
		0x0501: "server-error-operation-not-supported",
		0x0502: "server-error-service-unavailable",
		0x0503: "server-error-version-not-supported",
		0x0504: "server-error-device-error",
		0x0505: "server-error-temporary-error",
		0x0506: "server-error-not-accepting-jobs",
		0x0507: "server-error-busy",
		0x0508: "server-error-job-canceled",
	}
	n, ok := names[code]
	return n, ok
}

// MaxBody bounds the response body read by the client.
const MaxBody = 4 * 1024 * 1024

// AddKeyword adds a keyword attribute.
func (g *Group) AddKeyword(name, val string) {
	g.AddString(TagKeyword, name, val)
}
