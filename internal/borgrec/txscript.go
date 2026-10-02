package borgrec

import (
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/lightwebinc/bcommon/pushdrop"
)

// PushDropScript is the one canonical lock-before PushDrop for key, fields
// and an optional field signature (bcommon pushdrop.Script).
func PushDropScript(key *ec.PublicKey, fields [][]byte, sig []byte) []byte {
	return pushdrop.Script(key, fields, sig)
}

// FundingScript is a funding output locked to key: `<key> OP_CHECKSIG
// <62 6f 02> OP_DROP`, unsigned (spec section 9.4).
func FundingScript(key *ec.PublicKey) []byte {
	return PushDropScript(key, [][]byte{TagFunding}, nil)
}

// IsFundingShape reports a funding-shaped script under any key: the whole
// script is 0x21, 33 bytes, 0xac, 03 62 6f 02, 75 (Classify's funding).
func IsFundingShape(s []byte) bool { return Classify(s) == KindFunding }
