package borgrec

import (
	"bytes"

	"github.com/lightwebinc/bcommon/pushdrop"
	"github.com/lightwebinc/bcommon/record"
)

// Kind names what an output claims to be (spec section 11.1, Classify).
type Kind string

const (
	KindNone    Kind = ""
	KindGrant   Kind = "grant"
	KindWrap    Kind = "wrap"
	KindOrg     Kind = "org-token"
	KindGroup   Kind = "group-token"
	KindFunding Kind = "funding"
)

// Classify says what a locking script claims, reading nothing else of it. A
// script that claims something is then held to the exact script rebuilt from
// its fields (spec section 11.1); this is only the first look.
func Classify(s []byte) Kind {
	p, ok := pushdrop.FirstPush(s)
	if !ok {
		return KindNone
	}
	switch {
	case record.Claims(p, MagicGrant):
		return KindGrant
	case record.Claims(p, MagicWrap):
		return KindWrap
	case bytes.Equal(p, TagOrg):
		return KindOrg
	case bytes.Equal(p, TagGroup):
		return KindGroup
	case bytes.Equal(p, TagFunding) && len(s) == 35+4+1 && s[39] == 0x75:
		return KindFunding
	}
	return KindNone
}
