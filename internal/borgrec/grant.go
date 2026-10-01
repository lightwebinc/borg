package borgrec

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"sort"

	"github.com/bsv-blockchain/go-sdk/auth/certificates"
	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/cbor"
)

// Grant record keys.
const (
	MKMagic      = 0
	MKHall       = 1
	MKOrg        = 2
	MKSubject    = 3
	MKSerial     = 4
	MKRevocation = 5
	MKFields     = 6
	MKSignature  = 7
	MKKeyring    = 8
	MKReveal     = 9
	mkLast       = MKReveal
)

// Field ciphertext bounds: BRC-2's 32-byte IV and 16-byte tag around a value
// whose own bound is the field's rule.
var fieldCipherBounds = map[string][2]int{
	"expiry": {48 + len(ExpiryLayout), 48 + len(ExpiryLayout)},
	"role":   {49, 48 + MaxRole},
	"scope":  {49, 48 + MaxScope},
}

// Grant is one membership grant record: the payload of one grant carrier. It
// is a BRC-52 certificate (certifier the organisation, subject the member)
// laid out as CBOR, with the subject's master keyring and, unless the
// organisation is opaque, a public reveal of every field.
type Grant struct {
	Hall      string            // key 1
	Org       []byte            // key 2, the certifier and the carrier's owner
	Subject   []byte            // key 3, the member's identity key
	Serial    [32]byte          // key 4, the certificate serial number
	RevTxid   [32]byte          // key 5 [0], hash byte order
	RevVout   uint32            // key 5 [1]
	Fields    map[string][]byte // key 6, each field's BRC-2 ciphertext
	Signature []byte            // key 7, the certifier's DER signature
	Keyring   map[string][]byte // key 8, each field revelation key encrypted to the subject
	Reveal    map[string][]byte // key 9, each field revelation key in the clear; nil when absent
	Extra     cbor.Map          // integer keys above 9, preserved and ignored
}

// CheckDER refuses a signature that is not the one strict, low-S DER
// encoding of its (r, s): re-serializing the parsed value must give back the
// same bytes.
func CheckDER(sig []byte) error {
	if len(sig) < 8 || len(sig) > 72 {
		return ErrSignature
	}
	s, err := ec.ParseDERSignature(sig)
	if err != nil || !bytes.Equal(s.Serialize(), sig) {
		return ErrSignature
	}
	return nil
}

func checkNames(m map[string][]byte, what string) error {
	if len(m) != len(GrantFieldNames) {
		return fmt.Errorf("%w: %s", ErrFields, what)
	}
	for _, n := range GrantFieldNames {
		if _, ok := m[n]; !ok {
			return fmt.Errorf("%w: %s", ErrFields, what)
		}
	}
	return nil
}

// checkFields is the rule of key 6: exactly the three names, each
// ciphertext within its field's bound.
func checkFields(m map[string][]byte) error {
	if err := checkNames(m, "fields"); err != nil {
		return err
	}
	for n, c := range m {
		b := fieldCipherBounds[n]
		if len(c) < b[0] || len(c) > b[1] {
			return fmt.Errorf("%w: field %s length", ErrFields, n)
		}
	}
	return nil
}

// checkSized is the rule of keys 8 and 9: exactly the three names, each
// value exactly size bytes.
func checkSized(m map[string][]byte, size int, what string) error {
	if err := checkNames(m, what); err != nil {
		return err
	}
	for n, c := range m {
		if len(c) != size {
			return fmt.Errorf("%w: %s %s length", ErrFields, what, n)
		}
	}
	return nil
}

// Validate applies every rule that needs only the record itself.
func (g *Grant) Validate() error {
	defined := mkLast
	if g.Reveal != nil {
		defined++
	}
	if err := checkExtra(g.Extra, mkLast, defined); err != nil {
		return err
	}
	if err := CheckHall(g.Hall); err != nil {
		return err
	}
	if err := CheckOrgKey(g.Org); err != nil {
		return err
	}
	if err := CheckIdentity(g.Subject); err != nil {
		return err
	}
	if err := checkFields(g.Fields); err != nil {
		return err
	}
	if err := CheckDER(g.Signature); err != nil {
		return err
	}
	if err := checkSized(g.Keyring, KeyringEntryLen, "keyring"); err != nil {
		return err
	}
	if g.Reveal != nil {
		if err := checkSized(g.Reveal, RevealLen, "reveal"); err != nil {
			return err
		}
	}
	return g.cross()
}

func (g *Grant) cross() error {
	if bytes.Equal(g.Subject, g.Org) || IsAnyone(g.Subject) {
		return fmt.Errorf("%w: subject is the organisation or the anyone key", ErrMembers)
	}
	return nil
}

func textMap(m map[string][]byte) cbor.Map {
	out := make(cbor.Map, 0, len(m))
	for k, v := range m {
		out = append(out, cbor.Pair{Key: k, Val: append([]byte(nil), v...)})
	}
	return out
}

// Encode writes the grant record in canonical CBOR.
func (g *Grant) Encode() ([]byte, error) {
	if err := g.Validate(); err != nil {
		return nil, err
	}
	m := cbor.Map{
		{Key: uint64(MKMagic), Val: MagicGrant},
		{Key: uint64(MKHall), Val: g.Hall},
		{Key: uint64(MKOrg), Val: g.Org},
		{Key: uint64(MKSubject), Val: g.Subject},
		{Key: uint64(MKSerial), Val: append([]byte(nil), g.Serial[:]...)},
		{Key: uint64(MKRevocation), Val: []cbor.Value{append([]byte(nil), g.RevTxid[:]...), uint64(g.RevVout)}},
		{Key: uint64(MKFields), Val: textMap(g.Fields)},
		{Key: uint64(MKSignature), Val: g.Signature},
		{Key: uint64(MKKeyring), Val: textMap(g.Keyring)},
	}
	if g.Reveal != nil {
		m = append(m, cbor.Pair{Key: uint64(MKReveal), Val: textMap(g.Reveal)})
	}
	return encodeRecord(m, g.Extra, MaxGrantRecord)
}

// DecodeGrant parses a grant record in the refusal order. It checks the
// record's own rules; the certificate's signature, the revocation outpoint
// and the reveal are content rules (CheckCertificate, CheckRevocation,
// CheckReveal), applied after the carrier's.
func DecodeGrant(p []byte) (*Grant, error) {
	f, err := decodeRecord(p, MaxGrantRecord, mkLast, MagicGrant)
	if err != nil {
		return nil, err
	}
	g := &Grant{Extra: f.extra}
	if g.Hall, err = f.text(MKHall); err != nil {
		return nil, err
	}
	if err := CheckHall(g.Hall); err != nil {
		return nil, err
	}
	if g.Org, err = f.bytesN(MKOrg, -1); err != nil {
		return nil, err
	}
	if err := CheckOrgKey(g.Org); err != nil {
		return nil, err
	}
	if g.Subject, err = f.bytesN(MKSubject, -1); err != nil {
		return nil, err
	}
	if err := CheckIdentity(g.Subject); err != nil {
		return nil, err
	}
	b, err := f.bytesN(MKSerial, 32)
	if err != nil {
		return nil, err
	}
	copy(g.Serial[:], b)
	if _, err := f.need(MKRevocation); err != nil {
		return nil, err
	}
	rv, err := f.array(MKRevocation, MaxKeys)
	if err != nil || len(rv) != 2 {
		return nil, fmt.Errorf("%w: key %d", ErrType, MKRevocation)
	}
	txid, ok := rv[0].([]byte)
	vout, ok2 := rv[1].(uint64)
	if !ok || !ok2 {
		return nil, fmt.Errorf("%w: key %d", ErrType, MKRevocation)
	}
	if len(txid) != 32 || vout > MaxEpoch {
		return nil, fmt.Errorf("%w: key %d", ErrRange, MKRevocation)
	}
	copy(g.RevTxid[:], txid)
	g.RevVout = uint32(vout)
	if g.Fields, err = f.textMap(MKFields); err != nil {
		return nil, err
	}
	if err := checkFields(g.Fields); err != nil {
		return nil, err
	}
	if g.Signature, err = f.bytesN(MKSignature, -1); err != nil {
		return nil, err
	}
	if err := CheckDER(g.Signature); err != nil {
		return nil, err
	}
	if g.Keyring, err = f.textMap(MKKeyring); err != nil {
		return nil, err
	}
	if err := checkSized(g.Keyring, KeyringEntryLen, "keyring"); err != nil {
		return nil, err
	}
	if _, ok := f.known[MKReveal]; ok {
		if g.Reveal, err = f.textMap(MKReveal); err != nil {
			return nil, err
		}
		if err := checkSized(g.Reveal, RevealLen, "reveal"); err != nil {
			return nil, err
		}
	}
	if err := g.cross(); err != nil {
		return nil, err
	}
	return g, nil
}

// Certificate is the grant as a go-sdk BRC-52 certificate: the type this
// version defines, the serial, subject, certifier (the organisation),
// revocation outpoint, each field's ciphertext in standard padded base64
// (the spelling BRC-52's binary form signs), and the signature.
func (g *Grant) Certificate() (*certificates.Certificate, error) {
	subject, err := ec.PublicKeyFromBytes(g.Subject)
	if err != nil {
		return nil, ErrIdentity
	}
	org, err := ec.PublicKeyFromBytes(g.Org)
	if err != nil {
		return nil, ErrIdentity
	}
	fields := make(map[wallet.CertificateFieldNameUnder50Bytes]wallet.StringBase64, len(g.Fields))
	for n, c := range g.Fields {
		fields[wallet.CertificateFieldNameUnder50Bytes(n)] = wallet.StringBase64(base64.StdEncoding.EncodeToString(c))
	}
	h := chainhash.Hash(g.RevTxid)
	return certificates.NewCertificate(
		wallet.StringBase64(GrantTypeBase64()),
		wallet.StringBase64(base64.StdEncoding.EncodeToString(g.Serial[:])),
		*subject, *org,
		&transaction.Outpoint{Txid: h, Index: g.RevVout},
		fields, append([]byte(nil), g.Signature...),
	), nil
}

// Preimage is the BRC-52 binary form without the signature: the bytes the
// organisation signs.
func (g *Grant) Preimage() ([]byte, error) {
	c, err := g.Certificate()
	if err != nil {
		return nil, err
	}
	return c.ToBinary(false)
}

// CheckCertificate is the content rule grant-signature: the certificate's
// signature verifies under BRC-52 against the organisation as certifier.
func (g *Grant) CheckCertificate(ctx context.Context) error {
	c, err := g.Certificate()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCert, err)
	}
	if err := c.Verify(ctx); err != nil {
		return fmt.Errorf("%w: %v", ErrCert, err)
	}
	return nil
}

// CheckRevocation is the content rule grant-revocation: the certificate's
// revocation outpoint is the outpoint the carrier's one input spends, so
// sweeping the carrier's funding output revokes the certificate for every
// BRC-52 verifier.
func (g *Grant) CheckRevocation(inputTxid [32]byte, inputVout uint32) error {
	if g.RevTxid != inputTxid || g.RevVout != inputVout {
		return ErrRevocation
	}
	return nil
}

// CheckReveal is the content rule grant-reveal, applied when the record
// carries a reveal: every revealed key opens its field, and every value
// meets its rule. It returns the plaintext fields.
func (g *Grant) CheckReveal() (map[string]string, error) {
	if g.Reveal == nil {
		return nil, nil
	}
	out := make(map[string]string, len(g.Fields))
	for _, n := range GrantFieldNames {
		pt, err := gcmOpen(g.Reveal[n], g.Fields[n])
		if err != nil {
			return nil, fmt.Errorf("%w: %s does not decrypt", ErrReveal, n)
		}
		if err := CheckFieldValue(n, string(pt)); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrReveal, err)
		}
		out[n] = string(pt)
	}
	return out, nil
}

// GrantSpec is everything a grant is built from. The random values (field
// revelation keys, IVs) are inputs so that vectors are reproducible; a
// client draws them from a CSPRNG.
type GrantSpec struct {
	Hall       string
	Subject    *ec.PublicKey
	Serial     [32]byte
	RevTxid    [32]byte
	RevVout    uint32
	Values     map[string]string   // expiry, role, scope in the clear
	FieldKeys  map[string][32]byte // each field's revelation key
	FieldIVs   map[string][32]byte // each field ciphertext's IV
	KeyringIVs map[string][32]byte // each keyring entry's IV
	Reveal     bool                // publish the revelation keys
}

// BuildGrant issues a grant under the organisation's private key: each field
// encrypted under its revelation key (BRC-52), each revelation key encrypted
// to the subject under [2, "certificate field encryption"] with the field
// name as key id (the master keyring), and the certificate signed by the
// organisation under [2, "certificate signature"] (go-sdk's Sign). A client
// with only a BRC-100 wallet builds the same record through Encrypt and
// CreateSignature.
func BuildGrant(ctx context.Context, orgKey *ec.PrivateKey, s GrantSpec) (*Grant, error) {
	g := &Grant{
		Hall: s.Hall, Org: orgKey.PubKey().Compressed(), Subject: s.Subject.Compressed(),
		Serial: s.Serial, RevTxid: s.RevTxid, RevVout: s.RevVout,
		Fields: map[string][]byte{}, Keyring: map[string][]byte{},
	}
	if s.Reveal {
		g.Reveal = map[string][]byte{}
	}
	names := append([]string(nil), GrantFieldNames...)
	sort.Strings(names)
	kd := wallet.NewKeyDeriver(orgKey)
	for _, n := range names {
		v, ok := s.Values[n]
		if !ok {
			return nil, fmt.Errorf("%w: %s missing", ErrFields, n)
		}
		if err := CheckFieldValue(n, v); err != nil {
			return nil, err
		}
		fk := s.FieldKeys[n]
		c, err := gcmSeal(fk[:], s.FieldIVs[n], []byte(v))
		if err != nil {
			return nil, err
		}
		g.Fields[n] = c
		proto, keyID := certificates.GetCertificateEncryptionDetails(n, "")
		sk, err := kd.DeriveSymmetricKey(proto, keyID, wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: s.Subject})
		if err != nil {
			return nil, err
		}
		if g.Keyring[n], err = gcmSeal(sk.ToBytes(), s.KeyringIVs[n], fk[:]); err != nil {
			return nil, err
		}
		if s.Reveal {
			g.Reveal[n] = append([]byte(nil), fk[:]...)
		}
	}
	// A placeholder satisfies the record rules while the preimage is built;
	// the signature is not part of the preimage.
	c, err := g.Certificate()
	if err != nil {
		return nil, err
	}
	c.Signature = nil
	pw, err := wallet.NewCompletedProtoWallet(orgKey)
	if err != nil {
		return nil, err
	}
	if err := c.Sign(ctx, pw); err != nil {
		return nil, err
	}
	g.Signature = c.Signature
	if err := g.Validate(); err != nil {
		return nil, err
	}
	return g, nil
}

// OpenFields is the subject's side: it decrypts its master keyring through
// its own wallet (counterparty the organisation) and every field with it, as
// go-sdk's certificates.DecryptFields does for a stored certificate.
func (g *Grant) OpenFields(ctx context.Context, subject wallet.Interface) (map[string]string, error) {
	org, err := ec.PublicKeyFromBytes(g.Org)
	if err != nil {
		return nil, ErrIdentity
	}
	ring := make(map[wallet.CertificateFieldNameUnder50Bytes]wallet.StringBase64, len(g.Keyring))
	fields := make(map[wallet.CertificateFieldNameUnder50Bytes]wallet.StringBase64, len(g.Fields))
	for n, c := range g.Keyring {
		ring[wallet.CertificateFieldNameUnder50Bytes(n)] = wallet.StringBase64(base64.StdEncoding.EncodeToString(c))
	}
	for n, c := range g.Fields {
		fields[wallet.CertificateFieldNameUnder50Bytes(n)] = wallet.StringBase64(base64.StdEncoding.EncodeToString(c))
	}
	out, err := certificates.DecryptFields(ctx, subject, ring, fields,
		wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: org}, false, "")
	if err != nil {
		return nil, err
	}
	res := make(map[string]string, len(out))
	for k, v := range out {
		res[string(k)] = v
	}
	return res, nil
}
