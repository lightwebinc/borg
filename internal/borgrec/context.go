package borgrec

import (
	"fmt"

	"github.com/lightwebinc/bcommon/cbor"
	"github.com/lightwebinc/bcommon/commit"
)

// MaxPathSteps bounds an inclusion path: a roll of at most MaxRoll grants
// has a path of at most 16 steps.
const MaxPathSteps = 32

// MaxContext bounds an encoded inclusion context.
const MaxContext = 2048

// InclusionContext is the BRC-24 context a member answer carries for each
// grant (spec section 10.2): the organisation token whose roll was used, the
// grant's leaf index and the roll's size, and the RFC 6962 inclusion path.
type InclusionContext struct {
	Seq   uint64
	Index uint64
	Size  uint64
	Path  commit.Path
}

// Encode writes the context as canonical CBOR [seq, index, size, path], each
// step [bytes(32), bool], the bool true when the sibling is on the left.
func (c *InclusionContext) Encode() ([]byte, error) {
	if len(c.Path) > MaxPathSteps || c.Size == 0 || c.Index >= c.Size {
		return nil, ErrRange
	}
	steps := make([]cbor.Value, len(c.Path))
	for i, st := range c.Path {
		steps[i] = []cbor.Value{append([]byte(nil), st.Hash[:]...), st.Left}
	}
	return cbor.Encode([]cbor.Value{c.Seq, c.Index, c.Size, steps})
}

// DecodeContext parses a context, bounding it first.
func DecodeContext(b []byte) (*InclusionContext, error) {
	if len(b) > MaxContext {
		return nil, ErrTooLarge
	}
	v, err := cbor.DecodeValue(b)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCBOR, err)
	}
	a, ok := v.([]cbor.Value)
	if !ok || len(a) != 4 {
		return nil, ErrType
	}
	c := &InclusionContext{}
	var ok1, ok2, ok3 bool
	c.Seq, ok1 = a[0].(uint64)
	c.Index, ok2 = a[1].(uint64)
	c.Size, ok3 = a[2].(uint64)
	steps, ok4 := a[3].([]cbor.Value)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return nil, ErrType
	}
	if c.Size == 0 || c.Size > MaxRoll || c.Index >= c.Size || len(steps) > MaxPathSteps {
		return nil, ErrRange
	}
	for _, s := range steps {
		p, ok := s.([]cbor.Value)
		if !ok || len(p) != 2 {
			return nil, ErrType
		}
		h, ok := p[0].([]byte)
		left, ok2 := p[1].(bool)
		if !ok || !ok2 || len(h) != 32 {
			return nil, ErrType
		}
		var st commit.Step
		copy(st.Hash[:], h)
		st.Left = left
		c.Path = append(c.Path, st)
	}
	return c, nil
}

// ProveGrant builds the context for the grant at commitment within a roll.
func ProveGrant(seq uint64, roll [][32]byte, grant [32]byte) (*InclusionContext, error) {
	sorted := make([][32]byte, len(roll))
	copy(sorted, roll)
	sortCommitments(sorted)
	for i, c := range sorted {
		if c == grant {
			p, err := commit.Prove(sorted, i)
			if err != nil {
				return nil, err
			}
			return &InclusionContext{Seq: seq, Index: uint64(i), Size: uint64(len(sorted)), Path: p}, nil
		}
	}
	return nil, fmt.Errorf("%w: not in the roll", ErrRange)
}

// VerifyGrant checks a context against the roll root of the organisation
// token at its seq: the path takes the grant's commitment to the root. The
// leaf and node prefixes of RFC 6962 keep a path from passing a subtree off
// as a leaf, so membership needs no more; index and size are hints.
func (c *InclusionContext) VerifyGrant(grant, roll [32]byte) bool {
	return commit.Verify(grant, c.Path, roll)
}
