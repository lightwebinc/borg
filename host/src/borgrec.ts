/**
 * The byte-level contract of borg, the twin of the Go package
 * borgrec: the organisation and group records that ride mined chain
 * tokens, the membership grant and the epoch-key wrap that ride unmined
 * carriers, and the hall grammar. docs/spec.md is the normative text; the
 * vectors under testdata/vectors hold both codecs to the same bytes.
 *
 * Every record is canonical CBOR through @lightwebinc/bcommon, so one value
 * has one encoding a reader accepts. Decoding follows the refusal order of
 * spec section 3.1 step for step, so both codecs refuse a record for the
 * same reason.
 */
import { Hash, PrivateKey } from '@bsv/sdk'
import { CborMap, ascending, bytesEqual, compareBytes, recordReader, strictPublicKey, strictSignature, type Pair, type RecordFields, type Value } from '@lightwebinc/bcommon'
import { toHex } from './util.js'

/** The BRC-43 protocol, security level 1, and the key ids under it. */
export const ProtocolName = 'borg organisation'
export const Protocol: [1, string] = [1, ProtocolName]
export const KeyOrg = 'org'
export const KeyGroup = 'group'
export const KeyRecord = 'record'
export const KeyGrant = 'grant'
export const KeyChain = 'chain'
export const KeyFund = 'fund'

/** BRC-369's release protocol, security level 2, adopted. */
export const ReleaseProtocolName = 'keyed content release'
export const ReleaseProtocol: [2, string] = [2, ReleaseProtocolName]
export const DomainSymmetric = 'metanet keyed content symmetric v1'
export const DomainCommitment = 'metanet keyed content commitment v1'

/** The overlay names (BRC-87). */
export const TopicPrefix = 'tm_borg_'
export const LookupService = 'ls_borg'

/** The BRC-52 type of a membership grant: base64 of SHA-256 of this string. */
export const GrantTypeString = 'borg membership grant v1'

/** Tags and magics. The tag prefix "bo" belongs to borg. */
export const TagOrg = Uint8Array.of(0x62, 0x6f, 0x01)
export const TagFunding = Uint8Array.of(0x62, 0x6f, 0x02)
export const TagGroup = Uint8Array.of(0x62, 0x6f, 0x03)
export const MagicOrg = Uint8Array.of(0x62, 0x6f, 0x6f, 0x01)
export const MagicGroup = Uint8Array.of(0x62, 0x6f, 0x67, 0x01)
export const MagicGrant = Uint8Array.of(0x62, 0x6f, 0x6d, 0x01)
export const MagicWrap = Uint8Array.of(0x62, 0x6f, 0x77, 0x01)

/** Bounds. A reader accepts every record within them. */
export const MaxSafe = Number.MAX_SAFE_INTEGER
export const MaxTime = 253402300799
export const MaxEpoch = 0xffffffff
export const SuffixLen = 10
export const MaxHall = 50 - TopicPrefix.length
export const MaxHallName = MaxHall - 1 - SuffixLen
export const MinHall = 1 + 1 + SuffixLen
export const GroupIDLen = 16
export const MaxName = 64
export const MaxDelta = 64
export const MaxRoll = 65536
export const MaxPolicySet = 65536
export const MaxMembers = 1024
export const MaxEntries = 16
export const MaxOrgRecord = 8192
export const MaxGroupRecord = 8192
export const MaxGrantRecord = 4096
export const MaxWrapRecord = 4096
export const MaxKeys = 64
export const MaxRole = 64
export const MaxScope = 512
export const WrapLen = 182
export const KeyringEntryLen = 80
export const RevealLen = 32

/** The BRC-52 field names of a grant, in the order BRC-52 serializes them. */
export const GrantFieldNames = ['expiry', 'role', 'scope'] as const

/**
 * The fixed labels a host counts refusals by: the record reasons of spec
 * section 3.1, the content and member reasons, and the transaction reasons
 * of section 11.1.
 */
export type Reason =
  | 'too-large'
  | 'cbor'
  | 'key-type'
  | 'magic'
  | 'missing'
  | 'type'
  | 'range'
  | 'identity'
  | 'hall'
  | 'name'
  | 'list'
  | 'chain'
  | 'members'
  | 'fields'
  | 'signature-form'
  | 'entry'
  | 'grant-revocation'
  | 'grant-signature'
  | 'grant-reveal'
  | 'transition'
  | 'rotation'
  | 'count'
  | 'scope'
  | 'expiry'
  | 'role'
  | 'query'
  | 'token'
  | 'undecryptable'
  | 'commitment'
  | 'shape'
  | 'beef'
  | 'mineable'
  | 'unlock'
  | 'lock'
  | 'signature'
  | 'funding'
  | 'unmined'
  | 'token-input'
  | 'not-borg'

/** A refusal: the reason label, and a detail for people. */
export class Refusal extends Error {
  constructor(
    readonly reason: Reason,
    detail?: string,
  ) {
    super(detail === undefined ? `borgrec: ${reason}` : `borgrec: ${reason}: ${detail}`)
    this.name = 'Refusal'
  }
}

/** The reason of a thrown value: a Refusal's label, 'other' for anything else. */
export function reasonOf(e: unknown): Reason | 'other' {
  return e instanceof Refusal ? e.reason : 'other'
}

const sha256 = (b: Uint8Array): Uint8Array => Uint8Array.from(Hash.sha256(Array.from(b)))

/** The BRC-52 type of a membership grant, standard base64. */
export function grantTypeBase64(): string {
  return Buffer.from(sha256(new TextEncoder().encode(GrantTypeString))).toString('base64')
}

/** BRC-43's anyone key: the generator point. Nothing is ever released to it. */
export const AnyoneKey = Uint8Array.from(new PrivateKey(1).toPublicKey().encode(true) as number[])

export const isAnyone = (k: Uint8Array): boolean => bytesEqual(k, AnyoneKey)

/** A canonical compressed secp256k1 key (bcommon strictPublicKey), else identity. */
export function checkIdentity(k: Uint8Array): void {
  if (strictPublicKey(k) === undefined) throw new Refusal('identity')
}

/** The organisation's key rule: canonical, and never the anyone key. */
export function checkOrgKey(k: Uint8Array): void {
  if (strictPublicKey(k) === undefined || isAnyone(k)) throw new Refusal('identity')
}

/** A hall's readable part: 1 to 31 lowercase letters and single underscores, starting and ending with a letter. */
export function checkHallName(s: string): void {
  if (s.length === 0 || s.length > MaxHallName) throw new Refusal('hall')
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i)
    if (c >= 0x61 && c <= 0x7a) continue
    if (c === 0x5f && i !== 0 && i !== s.length - 1 && s.charCodeAt(i - 1) !== 0x5f) continue
    throw new Refusal('hall')
  }
}

/** The hall identifier grammar: <name>_<suffix>, suffix exactly ten lowercase letters. */
export function checkHall(s: string): void {
  if (s.length < MinHall || s.length > MaxHall) throw new Refusal('hall')
  const cut = s.length - SuffixLen - 1
  if (s.charCodeAt(cut) !== 0x5f) throw new Refusal('hall')
  for (let i = cut + 1; i < s.length; i++) {
    const c = s.charCodeAt(i)
    if (c < 0x61 || c > 0x7a) throw new Refusal('hall')
  }
  checkHallName(s.slice(0, cut))
}

/** A hall identifier's readable part and suffix. */
export function splitHall(s: string): { name: string; suffix: string } {
  checkHall(s)
  const cut = s.length - SuffixLen - 1
  return { name: s.slice(0, cut), suffix: s.slice(cut + 1) }
}

/**
 * Creates a hall identifier for name: name, an underscore and ten letters
 * drawn uniformly at random. random(n) returns n random bytes
 * (crypto.getRandomValues when omitted). The draw is the Go codec's: one byte
 * at a time, a byte of 234 or more (234 = 9 * 26) skipped, so the same bytes
 * give the same suffix in both languages.
 */
export function newHall(name: string, random?: (n: number) => Uint8Array): string {
  checkHallName(name)
  const draw = random ?? ((n: number) => crypto.getRandomValues(new Uint8Array(n)))
  let suffix = ''
  while (suffix.length < SuffixLen) {
    const b = draw(1)
    if (b.length !== 1) throw new Error('borgrec: short random read')
    if (b[0]! < 234) suffix += String.fromCharCode(0x61 + (b[0]! % 26))
  }
  return `${name}_${suffix}`
}

/** The topic manager name of a hall. */
export const topic = (hall: string): string => TopicPrefix + hall

// ---- the record reader, refusal steps 1 to 6 ----

type Fields = RecordFields

const rec = recordReader((reason, detail) => new Refusal(reason, detail))
const decodeRecord = rec.decode
const need = rec.need
const bytesN = rec.bytesN
const text = rec.text
const uint = rec.uint
const bool = rec.bool
const array = rec.array
const list32 = rec.list32

function textMap(f: Fields, k: number): Map<string, Uint8Array> {
  const v = need(f, k)
  if (!(v instanceof CborMap)) throw new Refusal('type', `key ${k}`)
  if (v.entries.length > GrantFieldNames.length) throw new Refusal('fields', `key ${k}`)
  const out = new Map<string, Uint8Array>()
  for (const p of v.entries) {
    if (typeof p.key !== 'string' || !(p.val instanceof Uint8Array)) throw new Refusal('fields', `key ${k}`)
    out.set(p.key, p.val)
  }
  return out
}

const utf8Len = (s: string): number => new TextEncoder().encode(s).length

/** The display-name rule: 1 to 64 bytes of UTF-8 text. */
function checkName(s: string): void {
  const n = utf8Len(s)
  if (n < 1 || n > MaxName) throw new Refusal('name')
}


function listKeys(f: Fields, k: number, max: number): Uint8Array[] {
  const a = array(f, k, max)
  const out = a.map((e) => {
    if (!(e instanceof Uint8Array)) throw new Refusal('list', `key ${k}`)
    checkIdentity(e)
    return e
  })
  if (!ascending(out)) throw new Refusal('list', `key ${k}`)
  return out
}

/**
 * Key k as a byte string or as CBOR null, which this codec reads as absent.
 * The key itself is required: a record that leaves it out is 'missing', so
 * every record writes every key this version defines.
 */
function nullableBytes(f: Fields, k: number): Uint8Array | null {
  const v = need(f, k)
  if (v === null) return null
  if (!(v instanceof Uint8Array)) throw new Refusal('type', `key ${k}`)
  return v
}

/**
 * The successor rule of both chain records (spec section 3.4): null, or an
 * identity key that is neither the anyone key nor the key it would succeed.
 */
function checkSuccessor(successor: Uint8Array | null, own: Uint8Array): void {
  if (successor === null) return
  checkIdentity(successor)
  if (isAnyone(successor) || bytesEqual(successor, own)) {
    throw new Refusal('identity', 'a successor that is the anyone key or the key it succeeds')
  }
}

/**
 * The succession rule both chains follow: a token carries its predecessor's
 * organisation key, or the key that predecessor named as its successor and
 * no other.
 */
function checkHandover(prevOrg: Uint8Array, prevSuccessor: Uint8Array | null, nextOrg: Uint8Array): void {
  if (bytesEqual(nextOrg, prevOrg)) return
  if (prevSuccessor !== null && bytesEqual(nextOrg, prevSuccessor)) return
  throw new Refusal('transition', 'the organisation key changed without the predecessor naming it')
}

const zero32 = (b: Uint8Array): boolean => b.every((x) => x === 0)
const has = (list: Uint8Array[], x: Uint8Array): boolean => list.some((y) => bytesEqual(x, y))

function finish(m: Pair[], extra: Pair[], max: number, last: number): Uint8Array {
  rec.checkExtra(extra, last, m.length)
  return rec.encode(m, extra, max)
}

const k = (n: number): bigint => BigInt(n)

// ---- the organisation record ----

export interface Org {
  hall: string
  org: Uint8Array
  seq: number
  /** Hash byte order; zero exactly at seq 0. */
  prev: Uint8Array
  name: string
  admit: Uint8Array[]
  revoke: Uint8Array[]
  count: number
  roll: Uint8Array
  opaque: boolean
  created: number
  /** The policy set, beside the roll and on the same chain. */
  define: Uint8Array[]
  retire: Uint8Array[]
  policyCount: number
  policy: Uint8Array
  /** The key that may carry the chain on; null is CBOR null. */
  successor: Uint8Array | null
  extra: Pair[]
}

function orgCross(o: Org): void {
  for (const a of o.admit) if (has(o.revoke, a)) throw new Refusal('list', 'a grant both admitted and revoked')
  if ((o.seq === 0) !== zero32(o.prev)) throw new Refusal('chain', 'prev is zero exactly at seq 0')
  for (const d of o.define) if (has(o.retire, d)) throw new Refusal('list', 'a policy record both defined and retired')
  if (o.seq === 0 && (o.revoke.length !== 0 || o.count !== o.admit.length)) throw new Refusal('count', 'the first token admits its whole roll')
  if (o.seq === 0 && (o.retire.length !== 0 || o.policyCount !== o.define.length)) throw new Refusal('count', 'the first token defines its whole policy set')
}

export function decodeOrg(b: Uint8Array): Org {
  const f = decodeRecord(b, MaxOrgRecord, 16, MagicOrg)
  const hall = text(f, 1)
  checkHall(hall)
  const org = bytesN(f, 2, -1)
  checkOrgKey(org)
  const seq = uint(f, 3, 0, MaxSafe)
  const prev = bytesN(f, 4, 32)
  const name = text(f, 5)
  checkName(name)
  const admit = list32(f, 6, MaxDelta)
  const revoke = list32(f, 7, MaxDelta)
  const count = uint(f, 8, 0, MaxRoll)
  const roll = bytesN(f, 9, 32)
  const opaque = bool(f, 10)
  const created = uint(f, 11, 1, MaxTime)
  const define = list32(f, 12, MaxDelta)
  const retire = list32(f, 13, MaxDelta)
  const policyCount = uint(f, 14, 0, MaxPolicySet)
  const policy = bytesN(f, 15, 32)
  const successor = nullableBytes(f, 16)
  checkSuccessor(successor, org)
  const o: Org = { hall, org, seq, prev, name, admit, revoke, count, roll, opaque, created, define, retire, policyCount, policy, successor, extra: f.extra }
  orgCross(o)
  return o
}

export function encodeOrg(o: Org): Uint8Array {
  const b = finish(
    [
      { key: 0n, val: MagicOrg },
      { key: 1n, val: o.hall },
      { key: 2n, val: o.org },
      { key: 3n, val: k(o.seq) },
      { key: 4n, val: o.prev },
      { key: 5n, val: o.name },
      { key: 6n, val: o.admit },
      { key: 7n, val: o.revoke },
      { key: 8n, val: k(o.count) },
      { key: 9n, val: o.roll },
      { key: 10n, val: o.opaque },
      { key: 11n, val: k(o.created) },
      { key: 12n, val: o.define },
      { key: 13n, val: o.retire },
      { key: 14n, val: k(o.policyCount) },
      { key: 15n, val: o.policy },
      { key: 16n, val: o.successor },
    ],
    o.extra,
    MaxOrgRecord,
    16,
  )
  decodeOrg(b)
  return b
}

/**
 * The admission rule an organisation token meets against its predecessor:
 * same hall and organisation, the next seq, prev naming the predecessor's
 * transaction (hash byte order), the same opaque, and the count.
 */
export function checkOrgTransition(prev: Org, prevTxid: Uint8Array, next: Org): void {
  if (next.seq === 0) throw new Refusal('transition', 'seq 0 spends no token')
  if (next.hall !== prev.hall || next.seq !== prev.seq + 1 || !bytesEqual(next.prev, prevTxid)) {
    throw new Refusal('transition', 'not the successor of the token spent')
  }
  checkHandover(prev.org, prev.successor, next.org)
  if (next.opaque !== prev.opaque) throw new Refusal('transition', 'opaque is fixed at the first token')
  if (prev.count + next.admit.length - next.revoke.length !== next.count) throw new Refusal('count')
  if (prev.policyCount + next.define.length - next.retire.length !== next.policyCount) throw new Refusal('count')
}

// ---- the group record ----

export interface Group {
  hall: string
  org: Uint8Array
  group: Uint8Array
  seq: number
  prev: Uint8Array
  name: string
  epoch: number
  epochSeq: number
  commitment: Uint8Array
  add: Uint8Array[]
  remove: Uint8Array[]
  count: number
  members: Uint8Array
  rotateOnJoin: boolean
  rotateEvery: number
  created: number
  /** The key that may carry this group's chain on; null is CBOR null. */
  successor: Uint8Array | null
  extra: Pair[]
}

export const beginsEpoch = (g: Group): boolean => g.epochSeq === g.seq

function groupCross(g: Group): void {
  for (const a of g.add) if (has(g.remove, a)) throw new Refusal('list', 'a member both added and removed')
  for (const a of g.add) if (bytesEqual(a, g.org) || isAnyone(a)) throw new Refusal('members')
  if ((g.seq === 0) !== zero32(g.prev)) throw new Refusal('chain', 'prev is zero exactly at seq 0')
  if (g.epochSeq > g.seq) throw new Refusal('chain', 'epochSeq after seq')
  if (g.seq === 0 && (g.epoch !== 0 || g.epochSeq !== 0)) throw new Refusal('chain', 'the first token begins epoch 0')
  if (g.seq === 0 && (g.remove.length !== 0 || g.count !== g.add.length)) throw new Refusal('count', 'the first token adds its whole membership')
}

export function decodeGroup(b: Uint8Array): Group {
  const f = decodeRecord(b, MaxGroupRecord, 17, MagicGroup)
  const hall = text(f, 1)
  checkHall(hall)
  const org = bytesN(f, 2, -1)
  checkOrgKey(org)
  const group = bytesN(f, 3, GroupIDLen)
  const seq = uint(f, 4, 0, MaxSafe)
  const prev = bytesN(f, 5, 32)
  const name = text(f, 6)
  checkName(name)
  const epoch = uint(f, 7, 0, MaxEpoch)
  const epochSeq = uint(f, 8, 0, MaxSafe)
  const commitment = bytesN(f, 9, 32)
  const add = listKeys(f, 10, MaxDelta)
  const remove = listKeys(f, 11, MaxDelta)
  const count = uint(f, 12, 0, MaxMembers)
  const members = bytesN(f, 13, 32)
  const rotateOnJoin = bool(f, 14)
  const rotateEvery = uint(f, 15, 0, MaxEpoch)
  const created = uint(f, 16, 1, MaxTime)
  const successor = nullableBytes(f, 17)
  checkSuccessor(successor, org)
  const g: Group = { hall, org, group, seq, prev, name, epoch, epochSeq, commitment, add, remove, count, members, rotateOnJoin, rotateEvery, created, successor, extra: f.extra }
  groupCross(g)
  return g
}

export function encodeGroup(g: Group): Uint8Array {
  const b = finish(
    [
      { key: 0n, val: MagicGroup },
      { key: 1n, val: g.hall },
      { key: 2n, val: g.org },
      { key: 3n, val: g.group },
      { key: 4n, val: k(g.seq) },
      { key: 5n, val: g.prev },
      { key: 6n, val: g.name },
      { key: 7n, val: k(g.epoch) },
      { key: 8n, val: k(g.epochSeq) },
      { key: 9n, val: g.commitment },
      { key: 10n, val: g.add },
      { key: 11n, val: g.remove },
      { key: 12n, val: k(g.count) },
      { key: 13n, val: g.members },
      { key: 14n, val: g.rotateOnJoin },
      { key: 15n, val: k(g.rotateEvery) },
      { key: 16n, val: k(g.created) },
      { key: 17n, val: g.successor },
    ],
    g.extra,
    MaxGroupRecord,
    17,
  )
  decodeGroup(b)
  return b
}

/**
 * The admission rule a group token meets against its predecessor: what
 * makes removal a rekey at every host.
 */
export function checkGroupTransition(prev: Group, prevTxid: Uint8Array, next: Group): void {
  if (next.seq === 0) throw new Refusal('transition', 'seq 0 spends no token')
  if (
    next.hall !== prev.hall ||
    !bytesEqual(next.group, prev.group) ||
    next.seq !== prev.seq + 1 ||
    !bytesEqual(next.prev, prevTxid)
  ) {
    throw new Refusal('transition', 'not the successor of the token spent')
  }
  checkHandover(prev.org, prev.successor, next.org)
  const mustRotate = next.remove.length > 0 || (prev.rotateOnJoin && next.add.length > 0)
  if (next.epoch === prev.epoch) {
    if (mustRotate) throw new Refusal('rotation')
    if (!bytesEqual(next.commitment, prev.commitment) || next.epochSeq !== prev.epochSeq) throw new Refusal('rotation', "the epoch's key changed without a new epoch")
  } else if (next.epoch === prev.epoch + 1) {
    if (bytesEqual(next.commitment, prev.commitment)) throw new Refusal('rotation', 'a new epoch reuses the key')
    if (next.epochSeq !== next.seq) throw new Refusal('rotation', 'a new epoch begins at this seq')
  } else {
    throw new Refusal('transition', 'epoch skips')
  }
  if (prev.count + next.add.length - next.remove.length !== next.count) throw new Refusal('count')
}

// ---- the membership grant ----

export interface Grant {
  hall: string
  org: Uint8Array
  subject: Uint8Array
  serial: Uint8Array
  /** The revocation outpoint: txid in hash byte order, and the output index. */
  revTxid: Uint8Array
  revVout: number
  fields: Map<string, Uint8Array>
  signature: Uint8Array
  keyring: Map<string, Uint8Array>
  /** Absent for an opaque organisation's grant. */
  reveal?: Map<string, Uint8Array>
  extra: Pair[]
}

const fieldCipherBounds: Record<string, [number, number]> = {
  expiry: [68, 68],
  role: [49, 48 + MaxRole],
  scope: [49, 48 + MaxScope],
}

function checkNames(m: Map<string, Uint8Array>, what: string): void {
  if (m.size !== GrantFieldNames.length || GrantFieldNames.some((n) => !m.has(n))) throw new Refusal('fields', what)
}

function checkFields(m: Map<string, Uint8Array>): void {
  checkNames(m, 'fields')
  for (const [n, c] of m) {
    const b = fieldCipherBounds[n]!
    if (c.length < b[0] || c.length > b[1]) throw new Refusal('fields', `field ${n} length`)
  }
}

function checkSized(m: Map<string, Uint8Array>, size: number, what: string): void {
  checkNames(m, what)
  for (const [n, c] of m) if (c.length !== size) throw new Refusal('fields', `${what} ${n} length`)
}

/**
 * The one strict, low-S DER encoding of an (r, s), 8 to 72 bytes.
 */
export const strictDER = strictSignature

function grantCross(g: Grant): void {
  if (bytesEqual(g.subject, g.org) || isAnyone(g.subject)) throw new Refusal('members', 'subject is the organisation or the anyone key')
}

export function decodeGrant(b: Uint8Array): Grant {
  const f = decodeRecord(b, MaxGrantRecord, 9, MagicGrant)
  const hall = text(f, 1)
  checkHall(hall)
  const org = bytesN(f, 2, -1)
  checkOrgKey(org)
  const subject = bytesN(f, 3, -1)
  checkIdentity(subject)
  const serial = bytesN(f, 4, 32)
  let rv: Value[]
  try {
    rv = array(f, 5, MaxKeys)
  } catch (e) {
    if (e instanceof Refusal && e.reason === 'missing') throw e
    throw new Refusal('type', 'key 5')
  }
  if (rv.length !== 2) throw new Refusal('type', 'key 5')
  const [txid, vout] = rv
  if (!(txid instanceof Uint8Array) || typeof vout !== 'bigint' || vout < 0n) throw new Refusal('type', 'key 5')
  if (txid.length !== 32 || vout > BigInt(MaxEpoch)) throw new Refusal('range', 'key 5')
  const fields = textMap(f, 6)
  checkFields(fields)
  const signature = bytesN(f, 7, -1)
  if (!strictDER(signature)) throw new Refusal('signature-form')
  const keyring = textMap(f, 8)
  checkSized(keyring, KeyringEntryLen, 'keyring')
  let reveal: Map<string, Uint8Array> | undefined
  if (f.known.has(9)) {
    reveal = textMap(f, 9)
    checkSized(reveal, RevealLen, 'reveal')
  }
  const g: Grant = { hall, org, subject, serial, revTxid: txid, revVout: Number(vout), fields, signature, keyring, reveal, extra: f.extra }
  grantCross(g)
  return g
}

const mapOf = (m: Map<string, Uint8Array>): CborMap => new CborMap([...m].map(([key, val]) => ({ key, val })))

export function encodeGrant(g: Grant): Uint8Array {
  const m: Pair[] = [
    { key: 0n, val: MagicGrant },
    { key: 1n, val: g.hall },
    { key: 2n, val: g.org },
    { key: 3n, val: g.subject },
    { key: 4n, val: g.serial },
    { key: 5n, val: [g.revTxid, k(g.revVout)] },
    { key: 6n, val: mapOf(g.fields) },
    { key: 7n, val: g.signature },
    { key: 8n, val: mapOf(g.keyring) },
  ]
  if (g.reveal !== undefined) m.push({ key: 9n, val: mapOf(g.reveal) })
  const b = finish(m, g.extra, MaxGrantRecord, 9)
  decodeGrant(b)
  return b
}

// ---- the wrap record ----

export interface WrapRecord {
  hall: string
  org: Uint8Array
  group: Uint8Array
  epoch: number
  epochId: Uint8Array
  entries: Uint8Array[]
  extra: Pair[]
}

/** BRC-78's version bytes as both SDKs write them. */
export const WrapVersion = Uint8Array.of(0x42, 0x42, 0x10, 0x33)

/** BRC-369 section 5.2's key id bytes: SHA-256(epoch id || epoch as 4 bytes big-endian). */
export function releaseKeyID(epochId: Uint8Array, epoch: number): Uint8Array {
  const e = new Uint8Array(4)
  new DataView(e.buffer).setUint32(0, epoch)
  const b = new Uint8Array(36)
  b.set(epochId, 0)
  b.set(e, 32)
  return sha256(b)
}

export interface Entry {
  sender: Uint8Array
  recipient: Uint8Array
  keyID: Uint8Array
  ciphertext: Uint8Array
}

/** Splits one wrap without checking it against a record. */
export function parseEntry(b: Uint8Array): Entry {
  if (b.length !== WrapLen || !bytesEqual(b.subarray(0, 4), WrapVersion)) throw new Refusal('entry')
  return { sender: b.subarray(4, 37), recipient: b.subarray(37, 70), keyID: b.subarray(70, 102), ciphertext: b.subarray(102) }
}

/** The host's rule for one wrap of a wrap record. */
export function checkEntry(b: Uint8Array, org: Uint8Array, epochId: Uint8Array, epoch: number): Entry {
  const e = parseEntry(b)
  if (!bytesEqual(e.sender, org)) throw new Refusal('entry', 'sender is not the organisation')
  if (strictPublicKey(e.recipient) === undefined || bytesEqual(e.recipient, org) || isAnyone(e.recipient)) throw new Refusal('entry', 'recipient')
  if (!bytesEqual(e.keyID, releaseKeyID(epochId, epoch))) throw new Refusal('entry', 'key id')
  return e
}

function checkEntries(w: WrapRecord): void {
  if (w.entries.length < 1 || w.entries.length > MaxEntries) throw new Refusal('list', 'entries')
  let last: Uint8Array | undefined
  for (const b of w.entries) {
    const e = checkEntry(b, w.org, w.epochId, w.epoch)
    if (last !== undefined && compareBytes(last, e.recipient) >= 0) throw new Refusal('entry', 'recipients not ascending')
    last = e.recipient
  }
}

export function decodeWrap(b: Uint8Array): WrapRecord {
  const f = decodeRecord(b, MaxWrapRecord, 6, MagicWrap)
  const hall = text(f, 1)
  checkHall(hall)
  const org = bytesN(f, 2, -1)
  checkOrgKey(org)
  const group = bytesN(f, 3, GroupIDLen)
  const epoch = uint(f, 4, 0, MaxEpoch)
  const epochId = bytesN(f, 5, 32)
  const entries = array(f, 6, MaxEntries).map((e) => {
    if (!(e instanceof Uint8Array)) throw new Refusal('entry')
    return e
  })
  const w: WrapRecord = { hall, org, group, epoch, epochId, entries, extra: f.extra }
  checkEntries(w)
  return w
}

export function encodeWrap(w: WrapRecord): Uint8Array {
  const b = finish(
    [
      { key: 0n, val: MagicWrap },
      { key: 1n, val: w.hall },
      { key: 2n, val: w.org },
      { key: 3n, val: w.group },
      { key: 4n, val: k(w.epoch) },
      { key: 5n, val: w.epochId },
      { key: 6n, val: w.entries },
    ],
    w.extra,
    MaxWrapRecord,
    6,
  )
  decodeWrap(b)
  return b
}

/** The recipients of a wrap record, in record order. */
export const recipients = (w: WrapRecord): Uint8Array[] => w.entries.map((e) => e.subarray(37, 70))

export const hexKey = (b: Uint8Array): string => toHex(b)
