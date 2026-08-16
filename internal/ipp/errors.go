package ipp

import (
	"pressguard/internal/domain"
)

// Classify maps a client error to a domain attempt ErrorCode so the
// scheduler/recovery layer can record a stable category. A nil error maps
// to ErrNone.
func Classify(err error) domain.ErrorCode {
	if err == nil {
		return domain.ErrNone
	}
	pe, ok := err.(*ProtoError)
	if !ok {
		return domain.ErrNetwork
	}
	switch pe.Kind {
	case ErrKindNetwork:
		return domain.ErrNetwork
	case ErrKindTimeout:
		return domain.ErrTimeout
	case ErrKindCanceled:
		return domain.ErrCanceled
	case ErrKindIPPStatus:
		// An IPP error status is a server-side failure class.
		return domain.ErrServer
	case ErrKindHTTP, ErrKindIllegalTag, ErrKindDuplicateScalar,
		ErrKindDuplicateGroup, ErrKindAttrWithoutGroup, ErrKindTruncated,
		ErrKindRequestID, ErrKindUnknownStatus, ErrKindOversized, ErrKindBadUTF8:
		return domain.ErrProtocol
	}
	return domain.ErrProtocol
}
