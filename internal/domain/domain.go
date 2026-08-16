// Package domain defines the immutable core abstractions of the
// PressGuard spooler: jobs, segments (physical-paper checkpoints),
// attempts, printer capabilities and the stable error vocabulary.
//
// The model is deliberately small and free of I/O. Persistence,
// scheduling and device interaction live in other packages and depend
// on these types. The central invariant is that physical printing is
// irreversible: a unit of output, once released, cannot be "rolled back"
// by a database transaction. Correctness therefore rests on persistent
// delivery identity, remote-state reconciliation, duplex paper boundaries
// and explicit human adjudication of uncertainty.
package domain

import (
	"fmt"
	"strconv"
	"strings"
)

// JobStatus is the lifecycle state of a job.
type JobStatus string

const (
	// JobPending: accepted, not yet leased by a worker and not blocked.
	JobPending JobStatus = "pending"
	// JobBlocked: no printer can satisfy the job's capability requirements.
	JobBlocked JobStatus = "blocked"
	// JobProcessing: a worker has leased the job and is driving a segment.
	JobProcessing JobStatus = "processing"
	// JobCompleted: all segments confirmed printed.
	JobCompleted JobStatus = "completed"
	// JobCanceled: canceled by request; no further segments will be submitted.
	JobCanceled JobStatus = "canceled"
	// JobFailed: a terminal failure that is not cancellation.
	JobFailed JobStatus = "failed"
)

// IsTerminal reports whether a status admits no further state transitions.
func (s JobStatus) IsTerminal() bool {
	switch s {
	case JobCompleted, JobCanceled, JobFailed:
		return true
	}
	return false
}

// JobSpec is the immutable request payload used to create a Job. It is
// separated from Job so the stored record can carry mutable scheduling
// state without losing the original intent.
type JobSpec struct {
	DocDigest   string // lowercase hex SHA-256 of the document bytes
	DocLength   int64  // exact byte length of the document
	TotalPages  int    // page count of the document
	Copies      int    // number of copies to produce
	Duplex      bool   // duplex printing (two logical pages per physical sheet)
	Media       string // required media, e.g. "A4"
	Color       string // required color mode, e.g. "color"
	Resolution  string // required resolution, e.g. "600dpi"
	Binding     string // required finishing, "" for none
	Format      string // document mime type, defaults to application/pdf
}

// Job is the persisted scheduling record for one document.
type Job struct {
	ID          string
	Spec        JobSpec
	Status      JobStatus
	Generation  int64 // monotonically incremented on cancel; fences in-flight work
	Revision    int64 // incremented on every persisted mutation (optimistic concurrency)
	MissingCaps []string
	CreatedAt   string // RFC3339
	UpdatedAt   string // RFC3339
}

// Format returns the job's document format, defaulting to PDF.
func (j *Job) Format() string {
	if j.Spec.Format != "" {
		return j.Spec.Format
	}
	return "application/pdf"
}

// SegmentStatus is the lifecycle state of a single physical-paper checkpoint.
type SegmentStatus string

const (
	// SegPending: not yet attempted.
	SegPending SegmentStatus = "pending"
	// SegProcessing: an attempt is in flight.
	SegProcessing SegmentStatus = "processing"
	// SegConfirmed: the printer acknowledged completion for this segment.
	SegConfirmed SegmentStatus = "confirmed"
	// SegUncertain: remote result could not be proven; blocks auto-reprint.
	SegUncertain SegmentStatus = "uncertain"
	// SegCanceled: canceled (typically because the owning job was canceled).
	SegCanceled SegmentStatus = "canceled"
)

// IsTerminal reports whether a segment status is settled.
func (s SegmentStatus) IsTerminal() bool {
	switch s {
	case SegConfirmed, SegUncertain, SegCanceled:
		return true
	}
	return false
}

// Segment represents one physical sheet's worth of output within a copy.
// For simplex jobs each page is its own segment; for duplex jobs each
// segment spans the front and back of one sheet (front=2k-1, back=2k).
// Recovery can only resume at segment boundaries: a duplex sheet's back
// cannot be reprinted independently of its front.
type Segment struct {
	JobID      string
	Seq        int    // 1-indexed global sequence within the job
	CopyIndex  int    // 1-indexed copy number
	PaperIndex int    // 1-indexed sheet within the copy
	PageRanges string // IPP page-ranges value, e.g. "1,2" or "3"
	BackBlank  bool   // duplex sheet whose back page does not exist
	Status     SegmentStatus
}

// PapersPerCopy reports the number of physical sheets a single copy of
// the document occupies.
func PapersPerCopy(totalPages int, duplex bool) int {
	if totalPages <= 0 {
		return 0
	}
	if duplex {
		return (totalPages + 1) / 2
	}
	return totalPages
}

// SegmentsFor expands a job spec into its ordered, deterministic list of
// segments. The ordering is copy-major: all sheets of copy 1 precede
// copy 2, and so on. This ordering defines the continuous safe prefix.
func SegmentsFor(spec JobSpec) []Segment {
	if spec.Copies <= 0 || spec.TotalPages <= 0 {
		return nil
	}
	papers := PapersPerCopy(spec.TotalPages, spec.Duplex)
	segs := make([]Segment, 0, spec.Copies*papers)
	for c := 1; c <= spec.Copies; c++ {
		for p := 1; p <= papers; p++ {
			var ranges string
			backBlank := false
			if spec.Duplex {
				front := 2*p - 1
				back := 2 * p
				if back > spec.TotalPages {
					ranges = strconv.Itoa(front)
					backBlank = true
				} else {
					ranges = strconv.Itoa(front) + "," + strconv.Itoa(back)
				}
			} else {
				ranges = strconv.Itoa(p)
			}
			segs = append(segs, Segment{
				Seq:        (c-1)*papers + p,
				CopyIndex:  c,
				PaperIndex: p,
				PageRanges: ranges,
				BackBlank:  backBlank,
				Status:     SegPending,
			})
		}
	}
	return segs
}

// SafePrefix returns the number of leading segments (1-indexed) that are
// confirmed. It is the largest k such that segments 1..k are all
// SegConfirmed. The resumable continuation point is segment k+1, but
// only if it is SegPending; a SegUncertain or SegProcessing segment at
// k+1 blocks all later segments.
func SafePrefix(segs []Segment) int {
	k := 0
	for _, s := range segs {
		if s.Status == SegConfirmed {
			k++
		} else {
			break
		}
	}
	return k
}

// ResumePoint describes where printing may safely continue.
type ResumePoint struct {
	CopyIndex  int
	PaperIndex int
	PageRanges string
	Reason     string // human-readable, e.g. "next pending segment"
	Blocked    bool   // true if a non-pending segment blocks continuation
}

// ResumeAfter returns the continuation point given a segment list. If the
// segment immediately after the safe prefix is pending, it is the resume
// point. If it is uncertain or processing, continuation is blocked.
func ResumeAfter(segs []Segment) ResumePoint {
	k := SafePrefix(segs)
	if k >= len(segs) {
		return ResumePoint{Reason: "all segments confirmed", Blocked: false}
	}
	next := segs[k]
	switch next.Status {
	case SegPending:
		return ResumePoint{
			CopyIndex:  next.CopyIndex,
			PaperIndex:  next.PaperIndex,
			PageRanges: next.PageRanges,
			Reason:     "next pending segment",
			Blocked:    false,
		}
	default:
		return ResumePoint{
			CopyIndex:  next.CopyIndex,
			PaperIndex: next.PaperIndex,
			PageRanges: next.PageRanges,
			Reason:     fmt.Sprintf("segment %d is %s; manual adjudication required", next.Seq, next.Status),
			Blocked:    true,
		}
	}
}

// RemoteState is the printer's view of a submitted job's state.
type RemoteState string

const (
	RemoteUnknown    RemoteState = "unknown"
	RemotePending    RemoteState = "pending"
	RemoteHeld       RemoteState = "held"
	RemoteProcessing RemoteState = "processing"
	RemoteCompleted  RemoteState = "completed"
	RemoteCanceled   RemoteState = "canceled"
	RemoteAborted    RemoteState = "aborted"
)

// IsTerminal reports whether a remote state is settled (no further
// transitions expected without a new attempt).
func (s RemoteState) IsTerminal() bool {
	switch s {
	case RemoteCompleted, RemoteCanceled, RemoteAborted:
		return true
	}
	return false
}

// ErrorCode categorises the failure mode recorded on an attempt.
type ErrorCode string

const (
	ErrNone      ErrorCode = ""
	ErrNetwork   ErrorCode = "network"
	ErrProtocol  ErrorCode = "protocol"
	ErrCapability ErrorCode = "capability"
	ErrTimeout   ErrorCode = "timeout"
	ErrCanceled  ErrorCode = "canceled"
	ErrServer    ErrorCode = "server"
)

// Attempt records one delivery attempt for a segment. The Token is the
// stable, unique IPP job-name; PrinterJobID is the remote identifier
// returned by Create-Job (0 if unknown, e.g. the response was lost).
// ReleaseFence is set once Release-Job has been issued; after the fence,
// a cancel cannot safely withdraw the job and must reconcile instead.
type Attempt struct {
	ID           string
	JobID        string
	SegmentSeq   int
	Token        string
	PrinterID    string
	PrinterJobID int64
	BytesSent    int64
	ReleaseFence bool
	RemoteState  RemoteState
	ErrorCode    ErrorCode
	AttemptCount int
	CreatedAt    string
	UpdatedAt    string
}

// TokenFor builds the stable delivery token for a (job, segment). It is
// used as the IPP job-name so that a lost Create-Job response can be
// recovered by querying the printer for a job with this name.
func TokenFor(jobID string, seq int) string {
	return fmt.Sprintf("pressguard:%s:%d", jobID, seq)
}

// Capability describes what a printer can do.
type Capability struct {
	Media      []string // supported media sizes, e.g. ["A4","Letter"]
	Color      []string // supported color modes, e.g. ["color","monochrome"]
	Resolution []string // supported resolutions, e.g. ["600dpi","1200dpi"]
	Duplex     bool     // supports two-sided printing
	Formats    []string // supported document formats, e.g. ["application/pdf"]
	Binding    []string // supported finishing, e.g. ["saddle-stitch","staple"]
}

// Supports reports whether the capability satisfies the job's requirements.
// When it does not, missing lists the human-readable, stable capability
// identifiers that are absent (e.g. "media=A4", "duplex").
func (c Capability) Supports(spec JobSpec) (ok bool, missing []string) {
	missing = nil
	if !contains(c.Media, spec.Media) {
		missing = append(missing, "media="+spec.Media)
	}
	if !contains(c.Color, spec.Color) {
		missing = append(missing, "color="+spec.Color)
	}
	if !contains(c.Resolution, spec.Resolution) {
		missing = append(missing, "resolution="+spec.Resolution)
	}
	format := spec.Format
	if format == "" {
		format = "application/pdf"
	}
	if !contains(c.Formats, format) {
		missing = append(missing, "format="+format)
	}
	if spec.Duplex && !c.Duplex {
		missing = append(missing, "duplex")
	}
	if spec.Binding != "" && !contains(c.Binding, spec.Binding) {
		missing = append(missing, "binding="+spec.Binding)
	}
	return len(missing) == 0, missing
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// JoinMissing renders a capability-mismatch detail list as a stable,
// comma-separated string suitable for storage and API responses.
func JoinMissing(missing []string) string {
	return strings.Join(missing, ",")
}
