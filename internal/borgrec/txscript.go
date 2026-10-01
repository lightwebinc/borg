package borgrec

import (
	"encoding/binary"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	sdkpushdrop "github.com/bsv-blockchain/go-sdk/transaction/template/pushdrop"
)

// op is one parsed script element: an opcode, and for a data push the bytes
// pushed.
type op struct {
	code byte
	data []byte
	push bool
}

// parseScript splits s into opcodes. A push whose declared length runs past
// the end refuses the whole script. The reader is this package's own rather
// than the SDK's, so that the Go and TypeScript codecs read every byte
// string the same way.
func parseScript(s []byte) ([]op, bool) {
	var out []op
	for i := 0; i < len(s); {
		c := s[i]
		i++
		var n int
		switch {
		case c == script.Op0:
			out = append(out, op{code: c, data: []byte{}, push: true})
			continue
		case c >= 1 && c <= 75:
			n = int(c)
		case c == script.OpPUSHDATA1:
			if i+1 > len(s) {
				return nil, false
			}
			n = int(s[i])
			i++
		case c == script.OpPUSHDATA2:
			if i+2 > len(s) {
				return nil, false
			}
			n = int(binary.LittleEndian.Uint16(s[i:]))
			i += 2
		case c == script.OpPUSHDATA4:
			if i+4 > len(s) {
				return nil, false
			}
			m := uint64(binary.LittleEndian.Uint32(s[i:]))
			i += 4
			if m > uint64(len(s)-i) {
				return nil, false
			}
			n = int(m)
		default:
			out = append(out, op{code: c})
			continue
		}
		if n > len(s)-i {
			return nil, false
		}
		out = append(out, op{code: c, data: s[i : i+n], push: true})
		i += n
	}
	return out, true
}

// pushFields reads s as a lock-before PushDrop, leniently: a 33-byte push,
// OP_CHECKSIG, then every data push up to the first opcode that is not one
// (the field signature included). Whether s is the canonical script is
// decided by rebuilding it (PushDropScript) and comparing bytes.
func pushFields(s []byte) ([][]byte, bool) {
	ops, ok := parseScript(s)
	if !ok || len(ops) < 3 || !ops[0].push || len(ops[0].data) != 33 || ops[1].code != script.OpCHECKSIG {
		return nil, false
	}
	var fields [][]byte
	for _, o := range ops[2:] {
		if !o.push {
			break
		}
		fields = append(fields, o.data)
	}
	return fields, len(fields) > 0
}

// PushDropScript is the one canonical lock-before PushDrop for key, fields
// and an optional field signature, exactly as bcommon pushdrop writes it:
// the 33-byte compressed key, OP_CHECKSIG, each field and then the
// signature as one minimal push, and the fewest OP_2DROP then OP_DROP that
// clear them. Every script a host checks is compared with this byte for
// byte (spec section 11.1).
func PushDropScript(key *ec.PublicKey, fields [][]byte, sig []byte) []byte {
	ops := []*script.ScriptChunk{{Op: 33, Data: key.Compressed()}, {Op: script.OpCHECKSIG}}
	all := append([][]byte{}, fields...)
	if sig != nil {
		all = append(all, sig)
	}
	for _, f := range all {
		ops = append(ops, sdkpushdrop.CreateMinimallyEncodedScriptChunk(f))
	}
	for n := len(all); n > 0; n -= 2 {
		if n == 1 {
			ops = append(ops, &script.ScriptChunk{Op: script.OpDROP})
		} else {
			ops = append(ops, &script.ScriptChunk{Op: script.Op2DROP})
		}
	}
	s, err := script.NewScriptFromScriptOps(ops)
	if err != nil {
		panic(err) // every chunk above is well formed
	}
	return *s
}

// FundingScript is a funding output locked to key: `<key> OP_CHECKSIG
// <62 6f 02> OP_DROP`, unsigned (spec section 9.4).
func FundingScript(key *ec.PublicKey) []byte {
	return PushDropScript(key, [][]byte{TagFunding}, nil)
}

// IsFundingShape reports a funding-shaped script under any key: the whole
// script is 0x21, 33 bytes, 0xac, 03 62 6f 02, 75 (Classify's funding).
func IsFundingShape(s []byte) bool { return Classify(s) == KindFunding }
