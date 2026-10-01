/**
 * The BEEF as declared on the wire, the twin of the Go
 * internal/borgrec/beef.go: its form, its BUMPs and its transactions in
 * order, before any parser merges two proofs of one block or collapses a
 * transaction listed twice; whether a path holds only the leaves it needs;
 * and the BEEF a chain token is admitted in.
 */
import { MerklePath, Transaction, Utils } from '@bsv/sdk'
import { toHex } from './util.js'

/** The host's BEEF bound when it names none: spec section 11.1's floor. */
export const DefaultMaxBEEF = 256 << 10
/** The host's sweep input bound when it names none. */
export const DefaultMaxSweepInputs = 1024

const V1 = 0xefbe0001
const V2 = 0xefbe0002
const ATOMIC = 0x01010101

export interface BeefEntry {
  /** Undefined for a txid-only entry. */
  tx?: Transaction
  /** Display order. */
  txid: string
  /** The index of its BUMP, or -1. */
  bump: number
  txidOnly: boolean
}

export interface WireBEEF {
  atomic: boolean
  /** The subject's txid, display order. */
  subject: string
  bumps: MerklePath[]
  entries: BeefEntry[]
  byTxid: Map<string, BeefEntry>
}

class Malformed extends Error {}

/**
 * Walks a BEEF of at most bound bytes: BEEF V1 (BRC-62), V2 (BRC-96) or
 * Atomic BEEF (BRC-95) around either, nothing after it. Each transaction's
 * inputs are linked to the earlier entries holding their sources and each
 * proven transaction to its BUMP. Paths are read as written, without the
 * SDK's offset and root checks, which the engine's SPV makes; the admission
 * rules hold them to what the object needs.
 */
export function readWire(raw: Uint8Array, bound: number): WireBEEF {
  if (raw.length > bound) throw new Malformed(`${raw.length} bytes, max ${bound}`)
  const r = new Utils.Reader(Array.from(raw))
  const u32 = (): number => {
    if (r.pos + 4 > raw.length) throw new Malformed('truncated')
    return r.readUInt32LE()
  }
  const varInt = (): number => {
    if (r.pos >= raw.length) throw new Malformed('truncated')
    return r.readVarIntNum()
  }
  const w: WireBEEF = { atomic: false, subject: '', bumps: [], entries: [], byTxid: new Map() }
  let v = u32()
  if (v === ATOMIC) {
    w.atomic = true
    if (r.pos + 32 > raw.length) throw new Malformed('truncated')
    w.subject = toHex(Uint8Array.from(r.read(32)).reverse())
    v = u32()
  }
  if (v !== V1 && v !== V2) throw new Malformed('version')
  const nb = varInt()
  if (nb > raw.length) throw new Malformed('count')
  for (let i = 0; i < nb; i++) w.bumps.push(MerklePath.fromReader(r, false, false))
  const nt = varInt()
  if (nt > raw.length) throw new Malformed('count')
  for (let i = 0; i < nt; i++) {
    const e: BeefEntry = { txid: '', bump: -1, txidOnly: false }
    let format = 0
    if (v === V2) {
      if (r.pos >= raw.length) throw new Malformed('truncated')
      format = r.readUInt8()
      if (format > 2) throw new Malformed('format')
    }
    if (format === 2) {
      if (r.pos + 32 > raw.length) throw new Malformed('truncated')
      e.txidOnly = true
      e.txid = toHex(Uint8Array.from(r.read(32)).reverse())
    } else {
      if (format === 1) e.bump = varInt()
      const tx = Transaction.fromReader(r)
      if (v === V1) {
        if (r.pos >= raw.length) throw new Malformed('truncated')
        const f = r.readUInt8()
        if (f > 1) throw new Malformed('flag')
        if (f === 1) e.bump = varInt()
      }
      if (e.bump >= w.bumps.length) throw new Malformed('bump index')
      if (e.bump >= 0) tx.merklePath = w.bumps[e.bump]
      e.tx = tx
      e.txid = tx.id('hex')
    }
    w.entries.push(e)
  }
  if (r.pos !== raw.length || w.entries.length === 0) throw new Malformed('trailing bytes or no transactions')
  if (!w.atomic) w.subject = w.entries.at(-1)!.txid
  for (const e of w.entries) {
    if (e.tx !== undefined) {
      if (e.tx.inputs.length === 0) throw new Malformed('a transaction of no inputs')
      for (const input of e.tx.inputs) {
        const src = w.byTxid.get(input.sourceTXID ?? '')
        if (src?.tx !== undefined) input.sourceTransaction = src.tx
      }
    }
    w.byTxid.set(e.txid, e)
  }
  if (w.byTxid.get(w.subject)?.tx === undefined) throw new Malformed('no subject transaction')
  return w
}

export const subjectTx = (w: WireBEEF): Transaction => w.byTxid.get(w.subject)!.tx!

export const anyTxidOnly = (w: WireBEEF): boolean => w.entries.some((e) => e.txidOnly)

/** txid at the lowest level, flagged as a txid. */
function txidOffset(mp: MerklePath, txid: string): number | undefined {
  return mp.path[0]?.find((e) => e.hash === txid && e.txid === true)?.offset
}

/**
 * Whether mp is the union of the minimal paths of a and b (a === b for one
 * transaction): at the lowest level a and b flagged as txids and nothing else
 * flagged, and their siblings; above that only the siblings of their
 * ancestors. A sibling that is itself an ancestor of the other may be left
 * out; every other sibling must be there; nothing else may be.
 */
export function mergedPath(mp: MerklePath, a: string, b: string): boolean {
  if (mp.path.length === 0) return false
  const sets: Array<Map<number, boolean>> = []
  for (const level of mp.path) {
    const m = new Map<number, boolean>()
    for (const e of level) {
      if (m.has(e.offset)) return false
      m.set(e.offset, e.txid === true)
    }
    sets.push(m)
  }
  const oa = txidOffset(mp, a)
  const ob = txidOffset(mp, b)
  if (oa === undefined || ob === undefined) return false
  let flagged = 0
  for (const t of sets[0]!.values()) if (t) flagged++
  if (flagged !== (a === b ? 1 : 2)) return false
  for (const [h, set] of sets.entries()) {
    const anc = new Set([Math.floor(oa / 2 ** h), Math.floor(ob / 2 ** h)])
    const allowed = new Set<number>()
    for (const x of anc) {
      allowed.add(x ^ 1)
      if (h === 0) allowed.add(x)
    }
    for (const off of set.keys()) if (!allowed.has(off)) return false
    for (const x of anc) {
      if (h === 0 && set.get(x) !== true) return false
      if (!set.has(x ^ 1) && !anc.has(x ^ 1)) return false
    }
  }
  return true
}

export const minimalPath = (mp: MerklePath, txid: string): boolean => mergedPath(mp, txid, txid)

function writeVarInt(n: number): number[] {
  const w = new Utils.Writer()
  w.writeVarIntNum(n)
  return w.toArray()
}

/**
 * The BEEF a chain token is admitted in (spec sections 11.1 and 11.6): a BEEF
 * V1 of exactly the parent then the token, each with its own proof, or one
 * path merged as the SDK merges two paths of one block. A replayer builds it
 * from the token as a host stores it and its parent.
 */
export function tokenBEEF(token: Transaction, parent: Transaction): Uint8Array {
  if (token.merklePath === undefined || parent.merklePath === undefined) throw new Error('borgrec: a token and its parent, both proven')
  const tp = MerklePath.fromBinary(token.merklePath.toBinary())
  const pp = MerklePath.fromBinary(parent.merklePath.toBinary())
  let bumps = [pp, tp]
  let idx = [0, 1]
  if (tp.blockHeight === pp.blockHeight && tp.computeRoot(token.id('hex')) === pp.computeRoot(parent.id('hex'))) {
    pp.combine(tp)
    bumps = [pp]
    idx = [0, 0]
  }
  const out: number[] = []
  const v = new Utils.Writer()
  v.writeUInt32LE(V1)
  out.push(...v.toArray(), ...writeVarInt(bumps.length))
  for (const m of bumps) out.push(...m.toBinary())
  out.push(...writeVarInt(2))
  for (const [i, tx] of [parent, token].entries()) {
    out.push(...tx.toBinary(), 1, ...writeVarInt(idx[i]!))
  }
  return Uint8Array.from(out)
}
