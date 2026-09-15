package webhook

import (
	"encoding/json"
	"strings"
)

// PushPayload is the subset of a Git provider push payload the webhook layer
// needs: the pushed ref, the new commit, and the create/delete flags. Only
// fields present in GitHub push events are read; extra fields are ignored.
type PushPayload struct {
	Ref     string `json:"ref"`
	Before  string `json:"before"`
	After   string `json:"after"`
	Created bool   `json:"created"`
	Deleted bool   `json:"deleted"`
}

// ParsePushPayload decodes a (already signature-verified) request body.
func ParsePushPayload(body []byte) (*PushPayload, error) {
	var p PushPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// BranchFromRef normalizes a Git ref to a bare branch name:
//
//	refs/heads/main → main
//	main            → main (already bare)
//	refs/tags/v1    → refs/tags/v1 (only branch refs are stripped)
//
// A tag ref therefore never matches a configured branch.
func BranchFromRef(ref string) string {
	return strings.TrimPrefix(ref, "refs/heads/")
}

// IsBranchDeletion reports whether the push deleted its ref — GitHub sets
// deleted=true and after to the all-zero SHA. There is nothing to rebuild.
func IsBranchDeletion(p *PushPayload) bool {
	if p == nil {
		return false
	}
	return p.Deleted || isNullSHA(p.After)
}

func isNullSHA(sha string) bool {
	if len(sha) != 40 && len(sha) != 64 {
		return false
	}
	for _, r := range sha {
		if r != '0' {
			return false
		}
	}
	return true
}
