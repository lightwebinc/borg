/**
 * The epoch key (BRC-369 sections 2 and 5) and a grant's content rules: the
 * twin of the Go internal/borgrec/epoch.go and the content half of
 * grant.go. A member unwraps its epoch key and checks it in the section 6.4
 * order; a host checks a grant's revocation outpoint, its certificate
 * signature and its reveal.
 */
import { Certificate, Hash, SymmetricKey, Utils, type ProtoWallet } from '@bsv/sdk'

/** What a member's checks need of its wallet: BRC-2 decryption. */
export type Decrypter = Pick<ProtoWallet, 'decrypt'>
import { bytesEqual } from '@lightwebinc/bcommon'
import {
  DomainCommitment,
  DomainSymmetric,
  GrantFieldNames,
  ReleaseProtocol,
  Refusal,
  beginsEpoch,
  checkEntry,
  decodeGroup,
  grantTypeBase64,
  releaseKeyID,
  type Grant,
  type Group,
  type WrapRecord,
} from './borgrec.js'
import { checkFieldValue } from './scope.js'
import { concat, displayTxid, toBase64, toHex } from './util.js'

const sha256 = (b: Uint8Array): Uint8Array => Uint8Array.from(Hash.sha256(Array.from(b)))
const text = new TextEncoder()
const n = 0xfffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141n

/** Refuses a 32-byte big-endian value outside [1, n-1]. */
export function checkScalar(k: Uint8Array): void {
  let v = 0n
  for (const b of k) v = (v << 8n) | BigInt(b)
  if (k.length !== 32 || v === 0n || v >= n) throw new Refusal('undecryptable', 'not a scalar in [1, n-1]')
}

/** BRC-369 section 2.1 rule 3. */
export const symmetricKey = (k: Uint8Array): Uint8Array => sha256(concat([text.encode(DomainSymmetric), k]))
/** BRC-369 section 2.2's digest form. */
export const commitmentOf = (k: Uint8Array): Uint8Array => sha256(concat([text.encode(DomainCommitment), k]))
/** SHA-256 of the encoded group record of the token that began the epoch. */
export const epochID = (groupRecord: Uint8Array): Uint8Array => sha256(groupRecord)
/** The release key id as the BRC-43 invoice carries it. */
export const releaseKeyIDString = (epochId: Uint8Array, epoch: number): string => toBase64(releaseKeyID(epochId, epoch))
export const releaseInvoice = (epochId: Uint8Array, epoch: number): string => `2-${ReleaseProtocol[1]}-${releaseKeyIDString(epochId, epoch)}`

/**
 * The member's check 1: the group record of the token that began the wrap's
 * epoch hashes to the epoch id and names the same hall, organisation, group
 * and epoch. The token's proof is the caller's.
 */
export function checkEpochToken(groupRecord: Uint8Array, w: WrapRecord): Group {
  let g: Group
  try {
    g = decodeGroup(groupRecord)
  } catch (e) {
    throw new Refusal('token', (e as Error).message)
  }
  if (
    !bytesEqual(epochID(groupRecord), w.epochId) ||
    !beginsEpoch(g) ||
    g.epoch !== w.epoch ||
    g.hall !== w.hall ||
    !bytesEqual(g.org, w.org) ||
    !bytesEqual(g.group, w.group)
  ) {
    throw new Refusal('token')
  }
  return g
}

/**
 * The member's side of BRC-369 sections 5.2 and 5.3: decrypt its wrap
 * through its own wallet (counterparty the organisation), refuse a value
 * that is not a scalar, and check it against the epoch's commitment.
 */
export async function unwrap(
  wallet: Decrypter,
  entry: Uint8Array,
  org: Uint8Array,
  epochId: Uint8Array,
  epoch: number,
  commitment: Uint8Array,
): Promise<Uint8Array> {
  const e = checkEntry(entry, org, epochId, epoch)
  let pt: number[]
  try {
    ;({ plaintext: pt } = await wallet.decrypt({
      ciphertext: Array.from(e.ciphertext),
      protocolID: ReleaseProtocol,
      keyID: releaseKeyIDString(epochId, epoch),
      counterparty: toHex(org),
    }))
  } catch (err) {
    throw new Refusal('undecryptable', (err as Error).message)
  }
  if (pt.length !== 32) throw new Refusal('undecryptable', `plaintext of ${pt.length} bytes`)
  const k = Uint8Array.from(pt)
  checkScalar(k)
  if (!bytesEqual(commitmentOf(k), commitment)) throw new Refusal('commitment')
  return k
}

// ---- a grant's content rules, after the carrier's ----

/** The grant as an SDK BRC-52 certificate. */
export function certificate(g: Grant): Certificate {
  const fields: Record<string, string> = {}
  for (const [name, c] of g.fields) fields[name] = toBase64(c)
  return new Certificate(
    grantTypeBase64(),
    toBase64(g.serial),
    toHex(g.subject),
    toHex(g.org),
    `${displayTxid(g.revTxid)}.${g.revVout}`,
    fields,
    toHex(g.signature),
  )
}

/** The BRC-52 binary form without the signature: the bytes the organisation signs. */
export const preimage = (g: Grant): Uint8Array => Uint8Array.from(certificate(g).toBinary(false))

/** grant-revocation: the revocation outpoint is the one the carrier's input spends (display txid). */
export function checkRevocation(g: Grant, inputTxid: string, inputVout: number): void {
  if (displayTxid(g.revTxid) !== inputTxid || g.revVout !== inputVout) throw new Refusal('grant-revocation')
}

/** grant-signature: the certificate verifies under BRC-52 against the organisation. */
export async function checkCertificate(g: Grant): Promise<void> {
  let ok = false
  try {
    ok = await certificate(g).verify()
  } catch {
    ok = false
  }
  if (!ok) throw new Refusal('grant-signature')
}

/** BRC-2's symmetric form: 32-byte IV, ciphertext, 16-byte tag. */
function gcmOpen(key: Uint8Array, sealed: Uint8Array): Uint8Array {
  if (sealed.length < 48) throw new Error('short')
  return Uint8Array.from(new SymmetricKey(Array.from(key)).decrypt(Array.from(sealed)) as number[])
}

/**
 * grant-reveal, when the record carries a reveal: every revealed key opens
 * its field, and every value meets its rule. Returns the values.
 */
export function checkReveal(g: Grant): Record<string, string> | undefined {
  if (g.reveal === undefined) return undefined
  const out: Record<string, string> = {}
  for (const name of GrantFieldNames) {
    let pt: Uint8Array
    try {
      pt = gcmOpen(g.reveal.get(name)!, g.fields.get(name)!)
    } catch {
      throw new Refusal('grant-reveal', `${name} does not decrypt`)
    }
    try {
      out[name] = checkFieldValue(name, pt)
    } catch (e) {
      throw new Refusal('grant-reveal', (e as Error).message)
    }
  }
  return out
}

/**
 * The subject's side: decrypt the master keyring through its own wallet
 * (counterparty the organisation, protocol [2, "certificate field
 * encryption"], key id the field name) and every field with it.
 */
export async function openFields(g: Grant, subject: Decrypter): Promise<Record<string, string>> {
  const out: Record<string, string> = {}
  for (const name of GrantFieldNames) {
    const { plaintext } = await subject.decrypt({
      ciphertext: Array.from(g.keyring.get(name)!),
      protocolID: [2, 'certificate field encryption'],
      keyID: name,
      counterparty: toHex(g.org),
    })
    out[name] = Utils.toUTF8(Array.from(gcmOpen(Uint8Array.from(plaintext), g.fields.get(name)!)))
  }
  return out
}
