package borgrec

import (
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/wallet"
)

// KeySigner derives the organisation's signer key: the key every instance
// of the organisation's tooling holds, because each holds the
// organisation's identity key, and that is a member of the organisation's
// groups like any other member (spec section 7.4). An epoch key wrapped to
// it is what a second instance, or a home rebuilt on a new machine, opens
// to hold the key another instance sampled.
const KeySigner = "signer"

var anyone = wallet.Counterparty{Type: wallet.CounterpartyTypeAnyone}

// SignerKey is the signer key of the organisation org, compressed: the
// BRC-42 child of org under [1, "borg organisation"], key id signer,
// counterparty anyone. Anyone derives it from the organisation's public
// key, so a reader can tell that a member is the organisation's own signer
// key; only the organisation derives its private key.
func SignerKey(org []byte) ([]byte, error) {
	pub, err := ec.PublicKeyFromBytes(org)
	if err != nil || CheckOrgKey(org) != nil {
		return nil, ErrIdentity
	}
	one := make([]byte, 32)
	one[31] = 1
	k, _ := ec.PrivateKeyFromBytes(one)
	child, err := wallet.NewKeyDeriver(k).DerivePublicKey(Protocol, KeySigner,
		wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: pub}, false)
	if err != nil {
		return nil, err
	}
	return child.Compressed(), nil
}

// SignerPrivate is the private key of the organisation's signer key,
// derived from the organisation's identity key.
func SignerPrivate(orgKey *ec.PrivateKey) (*ec.PrivateKey, error) {
	return wallet.NewKeyDeriver(orgKey).DerivePrivateKey(Protocol, KeySigner, anyone)
}
