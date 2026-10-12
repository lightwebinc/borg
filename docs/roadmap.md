# Roadmap

What borg is for, what this version does, what comes next and in what
order, and what is deliberately left open. Versions are plain semantic
versions; nothing here carries a date.

## What borg is for

borg is the directory and authorization substrate for services that decide
on someone else's behalf: who a key is, which organization and groups it
belongs to, what it may do there, and a signed record of what a service
decided about it. It is the shape of a cloud identity and access service
with no operator in the middle: every change is a mined transaction on the
organization's own chain, every reader verifies against its own block
headers, and overlay hosts replicate state without deciding anything.

## In this version (0.7.0)

- An organization as a key and a chain; membership as a revocable
  certificate whose revocation one spend makes visible to every verifier.
- Groups as chains with epochs: a shared key that changes whenever anyone
  leaves, enforced by every host.
- Roles and bindings on the organization's chain, with attenuation and
  expiry; effective permissions as a union with no deny rules.
- The decision procedure ([spec.md](spec.md) section 13), fail-closed, and
  the signed receipt that names the state it rested on.
- The `enforce` package: the whole procedure behind one call, with the
  receipt signed and returned, so a service adds authorization in one file.
- Key succession: a handover both keys sign.
- A reader that assembles the policy set from several hosts when none holds
  it whole, so running more hosts makes reads more available, not less.

## Next

Ordered by what unblocks the most.

1. **Delegated administration.** Today the organization's key signs every
   change, so every administrator is the root. The shape is already in the
   contract: succession has two keys signing one chain. The next version
   lets a token name signer keys, locks each token so any named signer can
   continue the chain, and names the `borg/` permission a signer must hold
   for each kind of change (spec section 5A.2 reserves the label now). A
   role carrying those permissions is how an organization delegates its
   administration without handing over its key, and how that key can live
   offline or in a hardware module and sign only to change the signers.
2. **Consumers.** bstore's gateway first, through an interface of its own
   with the `enforce` package behind it; then any service that admits a
   writer who is not the owner of what it writes to.
3. **Delegation.** A member letting a service act for it, for some of what
   it holds and for a bounded time, as a record of its own on the chain.
   The requester still authenticates at every hop; what travels is the
   fact of delegation, not a bearer token.
4. **Cross-organization access.** Today an organization admits an outside
   key by granting it membership, which is explicit and visible. A trust
   relationship between organizations, so that one can name a role another
   may hold, comes after delegation, since it is delegation between chains.

## Later, and areas of exploration

- **Conditions beyond expiry.** The permission grammar fixes syntax and
  nothing else, so conditions belong to the service that reads a
  permission. Two are natural on this platform and worth standard shapes:
  a permission that also requires a payment, and one that also requires a
  certificate field the subject discloses.
- **Faster policy changes.** One transition per block bounds how quickly a
  permission is granted (revocation is already immediate through a spend).
  Anchoring many transitions in one mined transaction is the known answer.
- **A conventional directory read surface.** The lookup classes already
  answer who is in what; an LDAP-shaped view over them would let existing
  tools read an organization without a client of their own.
- **Tree-based rekeying** for very large groups, where linear rekeying is
  the limit.
- **Hardware-backed keys.** Nothing in the pattern needs a key online except
  to sign a change. Signers and succession make it practical to keep the
  organization's key in a hardware module and sign rarely.
- **The receipt in TypeScript**, when a TypeScript service decides.

## What will not change

- No deny rules: a permission is held or it is not, and the fold is
  provable because nothing subtracts.
- Nothing decides offline: a role is not a token its holder presents.
- Hosts never decide: they replicate, and a reader holds them to the root.
- Membership and policy are public metadata; what a certificate says can be
  hidden, and nothing else.
