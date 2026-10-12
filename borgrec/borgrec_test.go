package borgrec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"math/rand"
	"testing"

	"github.com/bsv-blockchain/go-sdk/auth/certificates"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/wallet"
)

func key(b byte) *ec.PrivateKey {
	k, _ := ec.PrivateKeyFromBytes(bytes.Repeat([]byte{b}, 32))
	return k
}

func pw(t *testing.T, k *ec.PrivateKey) wallet.Interface {
	t.Helper()
	w, err := wallet.NewCompletedProtoWallet(k)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// TestWrapThroughWallets wraps through the organization's BRC-100 wallet with
// a random IV and unwraps through the member's, then checks that nobody
// else opens it and that the epoch's binding holds.
func TestWrapThroughWallets(t *testing.T) {
	ctx := context.Background()
	org, alice, bob := key(0x61), key(0x62), key(0x63)
	k, err := SampleKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	id := sha256.Sum256([]byte("an epoch's group record"))
	w, err := Wrap(ctx, pw(t, org), org.PubKey().Compressed(), alice.PubKey(), id, 7, k)
	if err != nil {
		t.Fatal(err)
	}
	if len(w) != WrapLen {
		t.Fatalf("wrap is %d bytes", len(w))
	}
	got, err := Unwrap(ctx, pw(t, alice), w, org.PubKey(), id, 7, Commitment(k))
	if err != nil || got != k {
		t.Fatalf("alice: %v", err)
	}
	if _, err := Unwrap(ctx, pw(t, bob), w, org.PubKey(), id, 7, Commitment(k)); err == nil {
		t.Fatal("bob opened alice's wrap")
	}
	if _, err := Unwrap(ctx, pw(t, alice), w, org.PubKey(), id, 8, Commitment(k)); err == nil {
		t.Fatal("a wrap opened under another epoch")
	}
	other, _ := SampleKey(nil)
	if _, err := Unwrap(ctx, pw(t, alice), w, org.PubKey(), id, 7, Commitment(other)); err != ErrCommitment {
		t.Fatalf("a key that misses the commitment: %v", err)
	}
	if _, err := Wrap(ctx, pw(t, org), org.PubKey().Compressed(), alice.PubKey(), id, 7, [32]byte{}); err == nil {
		t.Fatal("wrapped a zero scalar")
	}
	anyone, _ := ec.PublicKeyFromBytes(AnyoneKey())
	if _, err := Wrap(ctx, pw(t, org), org.PubKey().Compressed(), anyone, id, 7, k); err == nil {
		t.Fatal("wrapped to the anyone key")
	}
}

// TestWrapWithIVMatchesWallet: the vectors' deterministic wrap decrypts
// through a wallet, so the vectors pin what wallets produce.
func TestWrapWithIVMatchesWallet(t *testing.T) {
	org, alice := key(0x61), key(0x62)
	k, _ := SampleKey(nil)
	id := sha256.Sum256([]byte("x"))
	w, err := WrapWithIV(org, alice.PubKey(), id, 3, k, sha256.Sum256([]byte("iv")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Unwrap(context.Background(), pw(t, alice), w, org.PubKey(), id, 3, Commitment(k)); err != nil {
		t.Fatal(err)
	}
}

// TestSDKIssuedGrant: a certificate issued by go-sdk's own
// IssueCertificateForSubject, laid out as a grant record, verifies and opens;
// and a grant built here verifies as a go-sdk certificate.
func TestSDKIssuedGrant(t *testing.T) {
	ctx := context.Background()
	org, alice := key(0x61), key(0x62)
	rev := sha256.Sum256([]byte("tree"))
	mc, err := certificates.IssueCertificateForSubject(ctx, pw(t, org),
		wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: alice.PubKey()},
		map[string]string{"expiry": "2030-01-01T00:00:00Z", "role": "r", "scope": "member"},
		GrantTypeBase64(), func(string) (*transaction.Outpoint, error) {
			return &transaction.Outpoint{Txid: rev, Index: 4}, nil
		}, "")
	if err != nil {
		t.Fatal(err)
	}
	g := &Grant{Hall: "h_abcdefghij", Org: org.PubKey().Compressed(), Subject: alice.PubKey().Compressed(),
		RevTxid: rev, RevVout: 4, Fields: map[string][]byte{}, Keyring: map[string][]byte{}, Signature: mc.Signature}
	ser, _ := mc.SerialNumber.ToArray()
	g.Serial = ser
	for n, v := range mc.Fields {
		raw, err := decodeB64(string(v))
		if err != nil {
			t.Fatal(err)
		}
		g.Fields[string(n)] = raw
	}
	for n, v := range mc.MasterKeyring {
		raw, err := decodeB64(string(v))
		if err != nil {
			t.Fatal(err)
		}
		g.Keyring[string(n)] = raw
	}
	rec, err := g.Encode()
	if err != nil {
		t.Fatal(err)
	}
	d, err := DecodeGrant(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CheckCertificate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.CheckRevocation(rev, 4); err != nil {
		t.Fatal(err)
	}
	open, err := d.OpenFields(ctx, pw(t, alice))
	if err != nil || open["scope"] != "member" {
		t.Fatalf("open: %v %v", open, err)
	}
	// And a grant built here is a go-sdk certificate that verifies.
	spec := GrantSpec{Hall: "h_abcdefghij", Subject: alice.PubKey(), RevTxid: rev, RevVout: 1,
		Values:    map[string]string{"expiry": "2030-01-01T00:00:00Z", "role": "r", "scope": "member,admin"},
		FieldKeys: map[string][32]byte{}, FieldIVs: map[string][32]byte{}, KeyringIVs: map[string][32]byte{}, Reveal: true}
	for _, n := range GrantFieldNames {
		spec.FieldKeys[n] = sha256.Sum256([]byte("k" + n))
		spec.FieldIVs[n] = sha256.Sum256([]byte("f" + n))
		spec.KeyringIVs[n] = sha256.Sum256([]byte("r" + n))
	}
	mine, err := BuildGrant(ctx, org, spec)
	if err != nil {
		t.Fatal(err)
	}
	c, err := mine.Certificate()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	vals, err := certificates.DecryptFields(ctx, pw(t, alice), mapB64(mine.Keyring), mapB64(mine.Fields),
		wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: org.PubKey()}, false, "")
	if err != nil || vals["scope"] != "member,admin" {
		t.Fatalf("go-sdk DecryptFields: %v %v", vals, err)
	}
}

// TestDecodersSurviveGarbage feeds random and truncated bytes to every
// decoder and the classifier: they refuse, never panic.
func TestDecodersSurviveGarbage(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	seeds := [][]byte{{0xa0}, {0xbb, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, {0x9b, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}}
	for i := 0; i < 3000; i++ {
		b := make([]byte, r.Intn(300))
		r.Read(b)
		seeds = append(seeds, b)
	}
	for _, b := range seeds {
		_, _ = DecodeOrg(b)
		_, _ = DecodeGroup(b)
		_, _ = DecodeGrant(b)
		_, _ = DecodeWrap(b)
		_ = Classify(b)
		_, _ = ParseQuery(b)
		_, _ = ParseEntry(b)
		for n := range b {
			_ = Classify(b[:n])
		}
	}
}

func TestHall(t *testing.T) {
	h, err := NewHall("team_mail", nil)
	if err != nil || CheckHall(h) != nil || len(h) != len("team_mail")+11 {
		t.Fatalf("%q %v", h, err)
	}
	long := "abcdefghijabcdefghijabcdefghija" // 31
	if _, err := NewHall(long, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := NewHall(long+"b", nil); err == nil {
		t.Fatal("a 32-byte readable part")
	}
	for _, bad := range []string{"a_abcdefghi", "_a_abcdefghij", "a__b_abcdefghij", "A_abcdefghij", "a_abcdefghiJ", "a1_abcdefghij"} {
		if CheckHall(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if len(Topic(long+"_abcdefghij")) != 50 {
		t.Fatal("the longest topic is not 50 characters")
	}
}

func TestScalar(t *testing.T) {
	var zero, n [32]byte
	ec.S256().N.FillBytes(n[:])
	if CheckScalar(zero) == nil || CheckScalar(n) == nil {
		t.Fatal("0 or n accepted")
	}
	n[31]--
	if CheckScalar(n) != nil {
		t.Fatal("n-1 refused")
	}
}

func decodeB64(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }

func mapB64(m map[string][]byte) map[wallet.CertificateFieldNameUnder50Bytes]wallet.StringBase64 {
	out := map[wallet.CertificateFieldNameUnder50Bytes]wallet.StringBase64{}
	for k, v := range m {
		out[wallet.CertificateFieldNameUnder50Bytes(k)] = wallet.StringBase64(base64.StdEncoding.EncodeToString(v))
	}
	return out
}

// The signer key anyone derives from the organization's public key is the
// public key of the private key only the organization derives, and a wrap
// to it opens under that private key.
func TestSignerKey(t *testing.T) {
	orgKey, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	org := orgKey.PubKey().Compressed()
	pub, err := SignerKey(org)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := SignerPrivate(orgKey)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(priv.PubKey().Compressed(), pub) {
		t.Fatal("the public and private derivations of the signer key disagree")
	}
	if bytes.Equal(pub, org) || IsAnyone(pub) {
		t.Fatal("the signer key must be neither the organization nor the anyone key")
	}
	k, err := SampleKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	var id [32]byte
	id[0] = 7
	ow, _ := wallet.NewCompletedProtoWallet(orgKey)
	member, _ := ec.PublicKeyFromBytes(pub)
	entry, err := Wrap(context.Background(), ow, org, member, id, 3, k)
	if err != nil {
		t.Fatal(err)
	}
	sw, _ := wallet.NewCompletedProtoWallet(priv)
	got, err := Unwrap(context.Background(), sw, entry, orgKey.PubKey(), id, 3, Commitment(k))
	if err != nil || got != k {
		t.Fatalf("the signer key does not open its wrap: %v", err)
	}
	if _, err := SignerKey(AnyoneKey()); err == nil {
		t.Fatal("the anyone key has no signer key")
	}
}
