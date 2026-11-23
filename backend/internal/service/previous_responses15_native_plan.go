package service

import (
	"encoding/json"
)

type storedResponseNativePlan15 struct {
	previousID     string
	parentSnapshot []byte
}

// HasNativeParent15 identifies server-resolved local history after its public
// previous_response_id has been removed from the current upstream delta.
func (p *StoredResponsePlan) HasNativeParent15() bool {
	return p != nil && p.native15 != nil && previousResponseID15(p.native15.previousID) && len(p.native15.parentSnapshot) != 0
}

// NewStoredResponseNativePlan15 is selected only by the explicit native-capable
// server route. The public handler has already resolved/authorized previousID
// and removed it from the original current delta before calling this boundary.
// This tranche requires local storage; stateless native continuation is separate.
func NewStoredResponseNativePlan15(owner StoredResponseOwner, body []byte, previousID string, parentSnapshot []byte) (*StoredResponsePlan, error) {
	if !validStoredResponseOwner(owner) || (previousID == "") != (len(parentSnapshot) == 0) || previousID != "" && !previousResponseID15(previousID) {
		return nil, ErrStoredResponseInvalid
	}
	req, err := NormalizeStoredResponseNativeRequest15(body)
	if err != nil {
		return nil, err
	}
	if !req.Store {
		return nil, ErrStoredResponseUnsupported
	}
	return &StoredResponsePlan{Owner: owner, Request: req, ClientRequest: append(json.RawMessage(nil), req.UpstreamBody...), native15: &storedResponseNativePlan15{previousID: previousID, parentSnapshot: append([]byte(nil), parentSnapshot...)}}, nil
}
