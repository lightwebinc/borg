/**
 * The topic manager's transaction rules of spec section 11.1, the twin of
 * the Go internal/borgrec/admit.go: classification, then a carrier's rules 1
 * to 10, a token's rules 1 to 9, or the sweep rule. A transaction is refused
 * for the first rule it breaks, with the same reason label in both
 * languages, and every refusal is thrown: a topic manager raises it rather
 * than answer with empty instructions, so the engine records nothing for the
 * txid. No rule reads another topic or anything this topic holds (spec
 * section 11.1): the previous coins decide only retention.
 */
import { Hash, PublicKey, type ChainTracker, type Transaction, type TransactionOutput } from '@bsv/sdk'
import { LockTime, MaxSequence, bytesEqual, readerLockingKey, unlockingRefusal, verifyFieldSignature } from '@lightwebinc/bcommon'
import {
  KeyChain,
  KeyGrant,
  KeyGroup,
  KeyOrg,
  KeyRecord,
  Protocol,
  Refusal,
  checkGroupTransition,
  checkHall,
  checkOrgTransition,
  decodeGrant,
  decodeGroup,
  decodeOrg,
  strictDER,
  type Grant,
  type Group,
  type Org,
  type WrapRecord,
  decodeWrap,
} from './borgrec.js'
import { DefaultMaxBEEF, DefaultMaxSweepInputs, anyTxidOnly, mergedPath, minimalPath, readWire, subjectTx, type BeefEntry, type WireBEEF } from './beef.js'
import { checkCertificate, checkReveal, checkRevocation } from './epoch.js'
import { classify, firstPush, fundingScript, isFundingShape, pushDropScript, pushFields, type Kind } from './script.js'
import { concat, fromHex, toHex } from './util.js'

export interface Host {
  /** The topic's hall identifier. */
  hall: string
  /** A chain tracker over the host's own block headers. */
  headers?: ChainTracker
  /** The host's BEEF bound; DefaultMaxBEEF when omitted. */
  maxBEEF?: number
  /** The host's sweep input bound; DefaultMaxSweepInputs when omitted. */
  maxSweepInputs?: number
}

export interface Carrier {
  grant?: Grant
  wrap?: WrapRecord
  /** The record's org. */
  owner: Uint8Array
  /** The record exactly as carried. */
  record: Uint8Array
  /** C = txid(K), hash byte order. */
  commitment: Uint8Array
  /** The outpoint the carrier spends, txid in display order. */
  funding: { txid: string; vout: number }
}

export interface ChainToken {
  kind: 'org-token' | 'group-token'
  org?: Org
  group?: Group
  record: Uint8Array
  vout: number
}

export const seqOf = (t: ChainToken): number => (t.org ?? t.group)!.seq

export type TxKind = 'grant' | 'wrap' | 'org-token' | 'group-token' | 'sweep'

export interface Admission {
  kind: TxKind
  /** Hash byte order. */
  txid: Uint8Array
  outputs: number[]
  retain: number[]
  carrier?: Carrier
  token?: ChainToken
  /** The token at seq - 1, read from the parent the BEEF carries; absent at seq 0. */
  predecessor?: ChainToken
  /** The parent's txid, display order. */
  parent?: string
  /** For a sweep, every outpoint its inputs spend, txid in display order. */
  spent?: Array<{ txid: string; vout: number }>
}

const bin = (s: { toBinary(): number[] }): Uint8Array => Uint8Array.from(s.toBinary())
const hashOrder = (tx: Transaction): Uint8Array => Uint8Array.from(tx.hash() as number[])

/** Whether tx carries a proof that verifies against the headers. */
export async function mined(tx: Transaction, headers?: ChainTracker): Promise<boolean> {
  if (tx.merklePath === undefined || headers === undefined) return false
  try {
    return await tx.merklePath.verify(tx.id('hex'), headers)
  } catch {
    return false
  }
}

const keyIDOf: Record<string, string> = { grant: KeyGrant, wrap: KeyRecord, 'org-token': KeyOrg, 'group-token': KeyGroup }

/** The key an owner's output of a kind is locked to. */
function lockingKey(kind: Kind | 'chain', owner: Uint8Array): Uint8Array {
  const id = kind === 'chain' ? KeyChain : keyIDOf[kind]!
  return Uint8Array.from(readerLockingKey(Protocol, id, toHex(owner)).encode(true) as number[])
}

/** A field signature: strict low-S DER over SHA-256 of the fields, under key. */
function verifyField(key: Uint8Array, signed: Uint8Array, der: Uint8Array): boolean {
  if (!strictDER(der)) return false
  try {
    return verifyFieldSignature(PublicKey.fromString(toHex(key)), Array.from(signed), Array.from(der))
  } catch {
    return false
  }
}

function retained(tx: Transaction, held: number[]): number[] {
  for (const i of held) {
    if (!Number.isInteger(i) || i < 0 || i >= tx.inputs.length) throw new Error(`borgrec: previous coin names input ${i} of ${tx.inputs.length}`)
  }
  return [...new Set(held)].sort((a, b) => a - b)
}

/**
 * Applies spec section 11.1 to one submission: the BEEF as it arrived, and
 * held, the indices of its subject's inputs that spend an output the topic
 * holds. SPV of the BEEF is the engine's and is assumed to have run.
 */
export async function admit(raw: Uint8Array | number[], held: number[], h: Host): Promise<Admission> {
  try {
    checkHall(h.hall)
  } catch {
    throw new Error(`borgrec: host hall ${JSON.stringify(h.hall)} breaks the grammar`)
  }
  let w: WireBEEF
  try {
    w = readWire(Uint8Array.from(raw), h.maxBEEF ?? DefaultMaxBEEF)
  } catch (e) {
    throw new Refusal('beef', (e as Error).message)
  }
  const tx = subjectTx(w)
  const a: Admission = { kind: 'sweep', txid: hashOrder(tx), outputs: [], retain: retained(tx, held) }

  let claim = 'none' as Kind
  let claims = 0
  let at = 0
  for (const [i, o] of tx.outputs.entries()) {
    const k = classify(bin(o.lockingScript))
    if (k !== 'none' && k !== 'funding') {
      claim = k
      at = i
      claims++
    }
  }
  if (claims > 1) throw new Refusal('shape', `${claims} outputs claim a record or a token`)
  if (claim === 'grant' || claim === 'wrap') {
    a.carrier = await checkCarrier(w, tx, claim, h)
    a.kind = claim
    a.outputs = [0]
    return a
  }
  if (claim === 'org-token' || claim === 'group-token') {
    const r = await checkToken(w, tx, at, claim, h)
    a.kind = claim
    a.token = r.token
    a.predecessor = r.predecessor
    a.parent = r.parent
    a.outputs = [at]
    return a
  }

  // Sweep: output 0 funding-shaped under any key, and mined.
  const first = tx.outputs[0]
  if (first !== undefined && isFundingShape(bin(first.lockingScript))) {
    if (!(await mined(tx, h.headers))) throw new Refusal('unmined')
    if (anyTxidOnly(w) || w.entries.length !== 1 || w.bumps.length !== 1 || !minimalPath(w.bumps[0]!, tx.id('hex'))) {
      throw new Refusal('beef', 'a sweep is one transaction and its own minimal path')
    }
    if (tx.inputs.length > (h.maxSweepInputs ?? DefaultMaxSweepInputs)) throw new Refusal('too-large', `a sweep of ${tx.inputs.length} inputs`)
    a.outputs = [0]
    a.spent = tx.inputs.map((i) => ({ txid: i.sourceTXID ?? i.sourceTransaction!.id('hex'), vout: i.sourceOutputIndex }))
    return a
  }
  throw new Refusal('not-borg')
}

const srcTxid = (i: Transaction['inputs'][number]): string => i.sourceTXID ?? i.sourceTransaction!.id('hex')

/** The carrier's rules 1 to 10; 5 to 8 in bcommon carrier.Validate's order. */
async function checkCarrier(w: WireBEEF, tx: Transaction, claim: 'grant' | 'wrap', h: Host): Promise<Carrier> {
  // 1. shape.
  if (tx.inputs.length !== 1 || tx.outputs.length !== 1) throw new Refusal('shape', `${tx.inputs.length} inputs, ${tx.outputs.length} outputs`)
  const input = tx.inputs[0]!
  const out = tx.outputs[0]!
  const src = input.sourceTransaction?.outputs[input.sourceOutputIndex]
  if (src === undefined) throw new Refusal('beef', 'the spent output is not in the BEEF')
  if ((src.satoshis ?? 0) !== (out.satoshis ?? 0)) throw new Refusal('shape', 'value')
  // 2. beef.
  const parent = srcTxid(input)
  const self = w.byTxid.get(tx.id('hex'))
  const fund = w.byTxid.get(parent)
  if (anyTxidOnly(w) || w.entries.length !== 2 || w.bumps.length !== 1 || self === undefined || fund === undefined || self.bump !== -1 || fund.bump !== 0 || !minimalPath(w.bumps[0]!, parent)) {
    throw new Refusal('beef', "not the carrier and its funding tree's minimal path")
  }
  // 3. the record's own rules.
  const lock = bin(out.lockingScript)
  const record = firstPush(lock)!
  const base = { record, commitment: hashOrder(tx), funding: { txid: parent, vout: input.sourceOutputIndex } }
  let c: Carrier
  if (claim === 'grant') {
    const g = decodeGrant(record)
    c = { ...base, grant: g, owner: g.org }
  } else {
    const r = decodeWrap(record)
    c = { ...base, wrap: r, owner: r.org }
  }
  // 4. hall.
  if ((c.grant ?? c.wrap)!.hall !== h.hall) throw new Refusal('hall', "not this topic's hall")
  // 5. mineable.
  if (tx.lockTime < LockTime || (input.sequence ?? MaxSequence) === MaxSequence) throw new Refusal('mineable')
  // 6. unlock, bcommon's check.
  if (unlockingRefusal(tx) !== undefined) throw new Refusal('unlock')
  // 7. lock.
  const key = lockingKey(claim, c.owner)
  const fields = pushFields(lock)
  if (fields?.length !== 2) throw new Refusal('lock', 'want the record and a signature')
  const sig = fields[1]!
  if (!bytesEqual(lock, pushDropScript(key, [record], sig))) throw new Refusal('lock')
  // 8. signature.
  if (!verifyField(key, record, sig)) throw new Refusal('signature')
  // 9. funding.
  if (!bytesEqual(bin(src.lockingScript), fundingScript(key))) throw new Refusal('funding')
  // 10. a grant's content rules.
  if (c.grant !== undefined) {
    checkRevocation(c.grant, parent, input.sourceOutputIndex)
    await checkCertificate(c.grant)
    checkReveal(c.grant)
  }
  return c
}

/** A token's rule 2: the token and its parent, each proven minimally, never Atomic. */
function tokenBEEFRule(w: WireBEEF, tx: Transaction): BeefEntry {
  if (w.atomic || anyTxidOnly(w) || w.entries.length !== 2) throw new Refusal('beef', 'a token is a BEEF V1 or V2 of exactly the token and its parent')
  const txid = tx.id('hex')
  const self = w.byTxid.get(txid)
  const parent = w.entries.find((e) => e.txid !== txid)
  if (self === undefined || parent?.tx === undefined || self.bump < 0 || parent.bump < 0) throw new Refusal('beef', 'the token and its parent, each proven')
  if (!tx.inputs.some((i) => srcTxid(i) === parent.txid)) throw new Refusal('beef', 'no input spends the other transaction')
  if (w.bumps.length === 2) {
    if (w.bumps[0]!.blockHeight === w.bumps[1]!.blockHeight) throw new Refusal('beef', 'two paths of one block')
    if (self.bump === parent.bump || !minimalPath(w.bumps[self.bump]!, txid) || !minimalPath(w.bumps[parent.bump]!, parent.txid)) {
      throw new Refusal('beef', 'two minimal paths, one each')
    }
  } else if (w.bumps.length === 1) {
    if (!mergedPath(w.bumps[0]!, txid, parent.txid)) throw new Refusal('beef', 'one merged path flagging exactly both txids')
  } else {
    throw new Refusal('beef', `${w.bumps.length} BUMPs`)
  }
  return parent
}

/** One token output read well formed: fields, 1 satoshi, record, canonical lock, signature. */
export function readTokenOutput(o: TransactionOutput, vout: number): ChainToken | undefined {
  const s = bin(o.lockingScript)
  const kind = classify(s)
  const fields = pushFields(s)
  if ((kind !== 'org-token' && kind !== 'group-token') || fields?.length !== 3 || o.satoshis !== 1) return undefined
  try {
    const t: ChainToken = { kind, record: fields[1]!, vout }
    if (kind === 'org-token') t.org = decodeOrg(fields[1]!)
    else t.group = decodeGroup(fields[1]!)
    const key = lockingKey(kind, (t.org ?? t.group)!.org)
    if (!bytesEqual(s, pushDropScript(key, fields.slice(0, 2), fields[2])) || !verifyField(key, concat([fields[0]!, fields[1]!]), fields[2]!)) return undefined
    return t
  } catch {
    return undefined
  }
}

/** The one token a transaction holds, well formed, read without its parent (a stored token). */
export function readToken(tx: Transaction): ChainToken {
  let found: ChainToken | undefined
  for (const [i, o] of tx.outputs.entries()) {
    const k = classify(bin(o.lockingScript))
    if (k !== 'org-token' && k !== 'group-token') continue
    if (found !== undefined) throw new Refusal('shape', 'two token outputs')
    found = readTokenOutput(o, i)
    if (found === undefined) throw new Refusal('shape', 'not a well-formed token')
  }
  if (found === undefined) throw new Refusal('not-borg')
  return found
}

/** A chain token as a host stores it (the token alone with its proof), read without its parent. */
export function storedToken(raw: Uint8Array): { tx: Transaction; token: ChainToken } {
  let w: WireBEEF
  try {
    w = readWire(raw, DefaultMaxBEEF)
  } catch (e) {
    throw new Refusal('beef', (e as Error).message)
  }
  const tx = subjectTx(w)
  return { tx, token: readToken(tx) }
}

function samePredecessor(t: ChainToken, p: ChainToken): boolean {
  if (t.kind !== p.kind || seqOf(p) + 1 !== seqOf(t)) return false
  if (t.org !== undefined) return p.org!.hall === t.org.hall && bytesEqual(p.org!.org, t.org.org)
  return p.group!.hall === t.group!.hall && bytesEqual(p.group!.org, t.group!.org) && bytesEqual(p.group!.group, t.group!.group)
}

/** The token's rules 1 to 9. */
async function checkToken(w: WireBEEF, tx: Transaction, at: number, claim: 'org-token' | 'group-token', h: Host) {
  const out = tx.outputs[at]!
  const lock = bin(out.lockingScript)
  // 1. shape.
  const fields = pushFields(lock)
  if (fields?.length !== 3 || out.satoshis !== 1) throw new Refusal('shape', 'a token is two fields, a signature and 1 satoshi')
  // 2. beef.
  const pe = tokenBEEFRule(w, tx)
  const parent = pe.tx!
  // 3. the record's own rules.
  const t: ChainToken = { kind: claim, record: fields[1]!, vout: at }
  if (claim === 'org-token') t.org = decodeOrg(fields[1]!)
  else t.group = decodeGroup(fields[1]!)
  const rec = (t.org ?? t.group)!
  // 4. hall.
  if (rec.hall !== h.hall) throw new Refusal('hall', "not this topic's hall")
  // 5. lock.
  const key = lockingKey(claim, rec.org)
  if (!bytesEqual(lock, pushDropScript(key, fields.slice(0, 2), fields[2]))) throw new Refusal('lock')
  // 6. signature.
  if (!verifyField(key, concat([fields[0]!, fields[1]!]), fields[2]!)) throw new Refusal('signature')
  // 7. unmined.
  if (!(await mined(tx, h.headers)) || !(await mined(parent, h.headers))) throw new Refusal('unmined')
  // 8. token-input.
  const chainScript = fundingScript(lockingKey('chain', rec.org))
  let pred: ChainToken | undefined
  let spendsToken = 0
  let spendsChain = false
  for (const input of tx.inputs) {
    if (srcTxid(input) !== pe.txid || input.sourceOutputIndex >= parent.outputs.length) continue
    const po = parent.outputs[input.sourceOutputIndex]!
    const k = classify(bin(po.lockingScript))
    if (k === 'org-token' || k === 'group-token') {
      spendsToken++
      if (k === claim) pred = readTokenOutput(po, input.sourceOutputIndex) ?? pred
    } else if (k === 'funding' && bytesEqual(bin(po.lockingScript), chainScript)) {
      spendsChain = true
    }
  }
  const ptxidHash = fromHex(pe.txid).reverse()
  if (rec.seq === 0) {
    if (!spendsChain || spendsToken > 0) throw new Refusal('token-input', 'seq 0 spends a chain-tree output and no token')
    return { token: t, predecessor: undefined, parent: pe.txid }
  }
  if (!bytesEqual(ptxidHash, rec.prev) || spendsToken !== 1 || pred === undefined || !samePredecessor(t, pred)) {
    throw new Refusal('token-input', "the parent's token at seq - 1 is not spent exactly once")
  }
  // 9. the transition rules.
  if (claim === 'org-token') checkOrgTransition(pred.org!, ptxidHash, t.org!)
  else checkGroupTransition(pred.group!, ptxidHash, t.group!)
  return { token: t, predecessor: pred, parent: pe.txid }
}

/** Kept for symmetry with the Go Mined; a hash helper the tests use. */
export const sha256 = (b: Uint8Array): Uint8Array => Uint8Array.from(Hash.sha256(Array.from(b)))
