/**
 * `ls_borg`: one lookup service for every hall a host carries (spec sections
 * 10.2, 11.2 to 11.5).
 *
 * Two stores back it. The engine's storage holds the outputs the host
 * keeps: chain tokens, grant and wrap carriers, and sweep tombstones, which
 * this service indexes in memory and rebuilds from storage on start. The
 * journal (journal.ts) holds what the host keeps for its life beside them:
 * every carrier any hall admitted with the funding outpoint it spent, every
 * drop, every spend of such an outpoint by an admitted sweep, the first
 * sight of every token, and every held sweep whose tombstone was spent. Spends of outpoints no carrier used live only in a
 * bounded table, oldest dropped first, so a stranger's chain of mined
 * sweeps costs the host that table and nothing more.
 *
 * Every status is a function of what the host holds, never of arrival
 * order:
 *
 * - a carrier is `retracted` when an admitted sweep spends its funding
 *   outpoint, `superseded` when it is not its outpoint's winner (the lowest
 *   C in hash byte order) or the winner was dropped, and `held` otherwise;
 * - a token is `pending` while a token on its path to seq 0 is not held,
 *   and otherwise takes the status the fold of its chain gives it along
 *   that path (chain.ts): `consistent`, `roll-mismatch`,
 *   `members-mismatch`, `commitment-reuse` or `rejoin-unrotated`.
 *
 * The `roll`, `member` and `member-of` answers use the fold of a chain's
 * highest token that is not pending, the first mined among several.
 */
import { Transaction } from '@bsv/sdk'
import type {
  LookupFormula,
  LookupQuestion,
  LookupService,
  LookupServiceMetaData,
  ModuleHost,
  Output,
  OutputAdmittedByTopic,
  RestoreStorage,
} from '@lightwebinc/bcommon'
import { readTokenOutput } from './admit.js'
import { LookupService as ServiceName, Refusal, TopicPrefix, decodeGrant, decodePolicy, decodeWrap, recipients, topic, type Group, type Org } from './borgrec.js'
import { GroupFold, OrgFold, type TokenStatus } from './chain.js'
import type { CarrierLine, Journal, Line } from './journal.js'
import { encodeContext, proveGrant, provePolicy } from './merkle.js'
import { parseQuery, type Question } from './query.js'
import { classify, firstPush, isFundingShape } from './script.js'
import { displayTxid, fromHex, toHex } from './util.js'

/** The retention floor, from the moment the floor's clock starts (spec section 11.3). */
export const FloorDays = 31
/** Superseded carriers kept per outpoint as evidence (spec section 11.2). */
export const EvidenceWraps = 8
export const EvidenceGrants = 64
/** Tokens kept at one seq of one chain. */
export const MaxPerSeq = 8
/** The floor of the bounded table of spends of outpoints no carrier used. */
export const MinSpendTable = 65536

export type CarrierStatus = 'retracted' | 'superseded' | 'held'

/** A chain token the host holds. */
interface TokenEntry {
  txid: string
  vout: number
  kind: 'org' | 'group'
  hall: string
  org: string
  /** The group id in hex; empty for an organisation's chain. */
  group: string
  chain: string
  seq: number
  /** The predecessor's txid in display order; empty at seq 0. */
  prev: string
  rec: Org | Group
  height: number
  offset: number
}

/** One chain's tokens and its fold. */
interface ChainIdx {
  kind: 'org' | 'group'
  hall: string
  org: string
  group: string
  bySeq: Map<number, TokenEntry[]>
  byTxid: Map<string, TokenEntry>
  /** prev txid -> the txids of tokens naming it. */
  children: Map<string, Set<string>>
  status: Map<string, TokenStatus>
  /** The fold after a token, kept for tokens with no successor yet. */
  states: Map<string, OrgFold | GroupFold>
  /**
   * Tokens a full seq turned away while they were pending, by the txid they
   * name as their predecessor: txid -> output and seq. They stay in the
   * engine's storage, and are read again once that predecessor is decided.
   */
  turned: Map<string, Map<string, { vout: number; seq: number }>>
}

/** A grant, policy or wrap carrier the host holds. */
interface CarrierEntry {
  kind: 'g' | 'p' | 'w'
  c: string
  txid: string
  hall: string
  org: string
  op: string
  subject?: string
  group?: string
  epoch?: number
  recipients?: string[]
  /** A policy record's role, and what it binds to (a group id or a key). */
  role?: string
  to?: string
}

/** A sweep tombstone the host holds. */
interface SweepEntry {
  txid: string
  /** Every hall the sweep was admitted in: one mined sweep may be published to several topics. */
  halls: Set<string>
  spent: string[]
  height: number
  offset: number
  score: number
}

interface Spend {
  hall: string
  h: number
  at: number
}

/** One funding outpoint's row. */
interface Row {
  carriers: Map<string, CarrierLine>
  dropped: Set<string>
  /** Its winner was dropped: nothing on it is answered again. */
  closed: boolean
  /** Admitted sweeps that spend it. */
  sweeps: Map<string, Spend>
}

/** What restore and retention need from the engine's storage. */
export interface Storage extends RestoreStorage {
  findOutput: (txid: string, outputIndex: number, topic?: string, spent?: boolean, includeBEEF?: boolean) => Promise<Output | null>
  deleteOutput?: (txid: string, outputIndex: number, topic: string) => Promise<void>
}

/** The engine's lookup answer. */
export interface OutputList {
  type: 'output-list'
  outputs: Array<{ beef: number[]; outputIndex: number; context?: number[] }>
}

export interface Options {
  /** The hall identifiers the host carries. */
  halls: ReadonlySet<string>
  host: ModuleHost
  journal: Journal
  /** Priceable classes this host prices: asked through the engine, they are refused. */
  priced?: ReadonlySet<string>
  /** How long a host keeps what it no longer answers, in days; at least 31. */
  retentionDays?: number
  /** The bound on spends of outpoints no carrier used; at least 65536. */
  spendTable?: number
  /** The host's clock, Unix seconds. */
  now?: () => number
}

const outpoint = (txid: string, vout: number): string => `${txid}.${vout}`
const cmp = (a: string, b: string): number => (a < b ? -1 : a > b ? 1 : 0)
const srcTxid = (i: Transaction['inputs'][number]): string | undefined => i.sourceTXID ?? i.sourceTransaction?.id('hex')

function addTo<K>(m: Map<K, Set<string>>, k: K, v: string): void {
  let s = m.get(k)
  if (s === undefined) m.set(k, (s = new Set()))
  s.add(v)
}

function removeFrom<K>(m: Map<K, Set<string>>, k: K, v: string): void {
  const s = m.get(k)
  s?.delete(v)
  if (s?.size === 0) m.delete(k)
}

/** A proof's block height and the txid's position in its block. */
function place(tx: Transaction): { height: number; offset: number } {
  const mp = tx.merklePath
  if (mp === undefined) return { height: 0, offset: 0 }
  const leaf = mp.path[0]?.find((l) => l.txid === true && l.hash === tx.id('hex'))
  return { height: mp.blockHeight, offset: leaf?.offset ?? 0 }
}

/** First mined first: lower height, then lower position, then txid. */
const firstMined = (a: TokenEntry, b: TokenEntry): number => a.height - b.height || a.offset - b.offset || cmp(a.txid, b.txid)

export class BorgLookupService implements LookupService {
  /** The whole transaction: a carrier's funding outpoint, a token's predecessor and a sweep's spends are its inputs. */
  readonly admissionMode = 'whole-tx' as const
  /** Spends are read from admitted transactions, never from the engine's notices. */
  readonly spendNotificationMode = 'none' as const

  readonly halls: ReadonlySet<string>
  private readonly host: ModuleHost
  private readonly journal: Journal
  private readonly priced: ReadonlySet<string>
  private readonly retention: number
  private readonly tableCap: number
  private readonly now: () => number

  // What the journal holds.
  private readonly rows = new Map<string, Row>()
  private readonly lines = new Map<string, CarrierLine>()
  private readonly tokenSeen = new Map<string, number>()

  // What the host holds, from storage.
  private readonly chains = new Map<string, ChainIdx>()
  private readonly carriers = new Map<string, CarrierEntry>()
  private readonly grantsBySubject = new Map<string, Set<string>>()
  private readonly grantsByOrg = new Map<string, Set<string>>()
  private readonly policiesByOrg = new Map<string, Set<string>>()
  private readonly policiesByRole = new Map<string, Set<string>>()
  private readonly policiesByTo = new Map<string, Set<string>>()
  private readonly wrapsByEpoch = new Map<string, Set<string>>()
  private readonly wrapsByMember = new Map<string, Set<string>>()
  private readonly groupsByOrg = new Map<string, Set<string>>()
  private readonly sweeps = new Map<string, SweepEntry>()
  private readonly sweepsByOp = new Map<string, Set<string>>()
  /** Spends of outpoints no carrier used: outpoint -> sweep txid -> spend, oldest first. */
  private readonly table = new Map<string, Map<string, Spend>>()

  /** Held sweeps whose tombstone an admitted transaction spent, by txid, with the halls it happened in: storage no longer hands them over, so the journal names them. */
  private readonly spentSweeps = new Map<string, Set<string>>()
  /** Tokens to read again from storage: their predecessor is decided now. */
  private readonly reread: Array<{ hall: string; txid: string; vout: number }> = []

  /** Outputs dropped from the index and still to be deleted from storage. */
  private readonly pending = new Map<string, { txid: string; vout: number; topic: string }>()
  private storage: Storage | undefined
  /** While restoring, chains are folded and bounded once at the end rather than at each token. */
  private restoring = false
  /** The restore in flight: an admission waits for it. */
  private inflight: Promise<number> | undefined
  private warnedNoDelete = false
  /** A held carrier's txid -> its commitment. */
  private readonly carrierByTxid = new Map<string, string>()

  constructor(o: Options) {
    this.halls = o.halls
    this.host = o.host
    this.journal = o.journal
    this.priced = o.priced ?? new Set()
    const days = o.retentionDays ?? FloorDays
    if (!Number.isSafeInteger(days) || days < FloorDays) throw new Error(`ls_borg: retention of ${days} days is under the ${FloorDays}-day floor`)
    this.retention = days * 86400
    // The table's floor is held where an operator sets it (module.ts); a
    // test may name a smaller one to watch it fill.
    const cap = o.spendTable ?? MinSpendTable
    if (!Number.isSafeInteger(cap) || cap < 1) throw new Error(`ls_borg: a spend table of ${cap}`)
    this.tableCap = cap
    this.now = o.now ?? (() => Math.floor(Date.now() / 1000))
    for (const l of this.journal.load()) this.apply(l)
  }

  // ---- gauges

  get tokenCount(): number {
    let n = 0
    for (const ch of this.chains.values()) n += ch.byTxid.size
    return n
  }
  get grantCount(): number {
    return this.countOf('g')
  }
  get policyCount(): number {
    return this.countOf('p')
  }
  get wrapCount(): number {
    return this.carriers.size - this.grantCount - this.policyCount
  }
  private countOf(kind: 'g' | 'p' | 'w'): number {
    let n = 0
    for (const c of this.carriers.values()) if (c.kind === kind) n++
    return n
  }
  get sweepCount(): number {
    return this.sweeps.size
  }
  get rowCount(): number {
    return this.rows.size
  }
  /** Carriers ever admitted: one journal line, and one entry in memory, each, for the host's life. */
  get lineCount(): number {
    return this.lines.size
  }
  get tableSize(): number {
    return this.table.size
  }
  get pendingDeletes(): number {
    return this.pending.size
  }
  get pendingTokens(): number {
    let n = 0
    for (const ch of this.chains.values()) for (const t of ch.byTxid.values()) if (this.tokenStatusIn(ch, t) === 'pending') n++
    return n
  }

  // ---- the journal

  private row(op: string): Row {
    let r = this.rows.get(op)
    if (r === undefined) this.rows.set(op, (r = { carriers: new Map(), dropped: new Set(), closed: false, sweeps: new Map() }))
    return r
  }

  private apply(l: Line): void {
    switch (l.k) {
      case 'c': {
        this.row(l.op).carriers.set(l.c, l)
        this.lines.set(l.c, l)
        break
      }
      case 'd': {
        const r = this.row(l.op)
        r.dropped.add(l.c)
        if (l.w) r.closed = true
        break
      }
      case 's': {
        const r = this.row(l.op)
        if (!r.sweeps.has(l.s)) r.sweeps.set(l.s, { hall: l.hall, h: l.h, at: l.at })
        break
      }
      case 't': {
        if (!this.tokenSeen.has(l.txid)) this.tokenSeen.set(l.txid, l.at)
        break
      }
      case 'w': {
        addTo(this.spentSweeps, l.txid, l.hall)
        break
      }
    }
  }

  private queueDelete(txid: string, vout: number, hall: string): void {
    const t = topic(hall)
    this.pending.set(`${t}|${txid}.${vout}`, { txid, vout, topic: t })
  }

  private write(l: Line): void {
    this.journal.append(l)
    this.apply(l)
  }

  // ---- carrier statuses (spec section 11.2)

  /** The outpoint's winner: the lowest C, hash byte order, over every carrier ever admitted on it. */
  private winnerOf(op: string): string | undefined {
    const r = this.rows.get(op)
    if (r === undefined) return undefined
    let w: string | undefined
    for (const c of r.carriers.keys()) if (w === undefined || c < w) w = c
    return w
  }

  /** The status of the carrier with commitment c (hash byte order hex). */
  status(c: string): CarrierStatus | undefined {
    const l = this.lines.get(c)
    if (l === undefined) return undefined
    const r = this.rows.get(l.op)!
    if (r.sweeps.size > 0) return 'retracted'
    if (r.closed || this.winnerOf(l.op) !== c) return 'superseded'
    return 'held'
  }

  /** Whether the carrier with commitment c was dropped. */
  dropped(c: string): boolean {
    const l = this.lines.get(c)
    return l !== undefined && this.rows.get(l.op)!.dropped.has(c)
  }

  // ---- token statuses (spec section 11.2)

  private chainKey(hall: string, org: string, group: string): string {
    return `${hall}|${org}|${group === '' ? 'org' : group}`
  }

  /** The status of the token with txid (display order), when the host holds it. */
  tokenStatus(txid: string): TokenStatus | undefined {
    for (const ch of this.chains.values()) {
      const t = ch.byTxid.get(txid)
      if (t !== undefined) return this.tokenStatusIn(ch, t)
    }
    return undefined
  }

  /**
   * A token's status along its path to seq 0: pending when a token on it
   * is not held, otherwise what the fold gives it. A fold state is kept only
   * for tokens no held token succeeds; a later fork is folded again from
   * seq 0.
   */
  private tokenStatusIn(ch: ChainIdx, t: TokenEntry): TokenStatus {
    const memo = ch.status.get(t.txid)
    if (memo !== undefined) return memo
    const path: TokenEntry[] = []
    let base: OrgFold | GroupFold | undefined
    let owner: TokenEntry | undefined
    let cur = t
    for (;;) {
      const s = cur === t ? undefined : ch.states.get(cur.txid)
      if (s !== undefined) {
        base = s.clone()
        owner = cur
        break
      }
      path.push(cur)
      if (cur.seq === 0) {
        base = ch.kind === 'org' ? new OrgFold() : new GroupFold()
        break
      }
      const p = ch.byTxid.get(cur.prev)
      if (p === undefined || p.seq !== cur.seq - 1) {
        for (const x of path) ch.status.set(x.txid, 'pending')
        return 'pending'
      }
      cur = p
    }
    for (const x of path.reverse()) {
      const known = ch.status.get(x.txid)
      const s = base.apply(x.rec as Org & Group)
      if (known === undefined || known === 'pending') this.counted(ch, x, s)
      ch.status.set(x.txid, s)
    }
    ch.states.set(t.txid, base)
    if (owner !== undefined) this.release(ch, owner)
    return ch.status.get(t.txid)!
  }

  /** Lets go of a token's fold once every token naming it is decided. */
  private release(ch: ChainIdx, t: TokenEntry): void {
    for (const c of ch.children.get(t.txid) ?? []) {
      const s = ch.status.get(c)
      if (s === undefined || s === 'pending') return
    }
    ch.states.delete(t.txid)
  }

  private counted(ch: ChainIdx, t: TokenEntry, s: TokenStatus): void {
    this.host.metrics.inc('borg_token_status_total', { status: s })
    if (s !== 'consistent') {
      this.host.log('ls_borg token status', { hall: ch.hall, org: ch.org, chain: ch.group === '' ? 'org' : ch.group, seq: t.seq, txid: t.txid, status: s })
    }
  }

  /**
   * Every token of the chain whose status was pending, decided again. A
   * token a full seq turned away while it was pending is queued to be read
   * again once the predecessor it names is decided, when it is that
   * predecessor's next seq; one at any other seq stays pending for good and
   * is let go.
   */
  private refold(ch: ChainIdx): void {
    for (const [txid, s] of [...ch.status]) if (s === 'pending') ch.status.delete(txid)
    for (const t of ch.byTxid.values()) this.tokenStatusIn(ch, t)
    for (const [prev, m] of [...ch.turned]) {
      const p = ch.byTxid.get(prev)
      if (p === undefined || ch.status.get(prev) === 'pending') continue
      ch.turned.delete(prev)
      for (const [txid, t] of m) if (t.seq === p.seq + 1) this.reread.push({ hall: ch.hall, txid, vout: t.vout })
    }
  }

  /**
   * Holds a seq to its bound: tokens that are not pending before pending
   * ones, then the lowest txids. So what is kept is a function of what the
   * host holds, and a token on a predecessor no host has cannot take the
   * place of one on the chain. A pending token turned away is remembered by
   * its predecessor.
   */
  private bound(ch: ChainIdx, seq: number): void {
    const at = ch.bySeq.get(seq)
    if (at === undefined || at.length <= MaxPerSeq) return
    const pending = new Map(at.map((t) => [t.txid, this.tokenStatusIn(ch, t) === 'pending']))
    const order = [...at].sort((a, b) => Number(pending.get(a.txid)) - Number(pending.get(b.txid)) || cmp(a.txid, b.txid))
    for (const t of order.slice(MaxPerSeq)) {
      this.host.log('ls_borg kept no more tokens at this seq', { hall: ch.hall, txid: t.txid, seq })
      if (pending.get(t.txid) === true && t.prev !== '') {
        let m = ch.turned.get(t.prev)
        if (m === undefined) ch.turned.set(t.prev, (m = new Map()))
        m.set(t.txid, { vout: t.vout, seq: t.seq })
      }
      this.forgetToken(ch, t)
    }
    this.refold(ch)
  }

  /** The highest token that is not pending, the first mined among several. */
  private answerToken(ch: ChainIdx): TokenEntry | undefined {
    let best: TokenEntry | undefined
    for (const t of ch.byTxid.values()) {
      if (this.tokenStatusIn(ch, t) === 'pending') continue
      if (best === undefined || t.seq > best.seq || (t.seq === best.seq && firstMined(t, best) < 0)) best = t
    }
    return best
  }

  /** The fold after t (not pending), folded again from seq 0 when it was not kept. */
  private foldAt(ch: ChainIdx, t: TokenEntry): OrgFold | GroupFold {
    const kept = ch.states.get(t.txid)
    if (kept !== undefined) return kept
    const path: TokenEntry[] = []
    for (let cur: TokenEntry | undefined = t; cur !== undefined; cur = cur.seq === 0 ? undefined : ch.byTxid.get(cur.prev)) path.push(cur)
    const f = ch.kind === 'org' ? new OrgFold() : new GroupFold()
    for (const x of path.reverse()) f.apply(x.rec as Org & Group)
    return f
  }

  /** The token path from t down to seq 0, t first. */
  private pathOf(ch: ChainIdx, t: TokenEntry): TokenEntry[] {
    const out: TokenEntry[] = []
    for (let cur: TokenEntry | undefined = t; cur !== undefined; cur = cur.seq === 0 ? undefined : ch.byTxid.get(cur.prev)) out.push(cur)
    return out
  }

  /** The roll the answers use for an organisation, with the seq of the token it is after. */
  private answerRoll(hall: string, org: string): { seq: number; roll: Uint8Array[]; set: Set<string>; path: TokenEntry[] } | undefined {
    const ch = this.chains.get(this.chainKey(hall, org, ''))
    if (ch === undefined) return undefined
    const t = this.answerToken(ch)
    if (t === undefined) return undefined
    const roll = (this.foldAt(ch, t) as OrgFold).rollNow()
    return { seq: t.seq, roll, set: new Set(roll.map(toHex)), path: this.pathOf(ch, t) }
  }

  /**
   * The policy set of the organisation's answerable token, the set a role,
   * bound or policy answer is taken from and the root its context proves
   * against.
   */
  private answerPolicy(hall: string, org: string): { seq: number; set: Uint8Array[]; has: Set<string>; path: TokenEntry[] } | undefined {
    const ch = this.chains.get(this.chainKey(hall, org, ''))
    if (ch === undefined) return undefined
    const t = this.answerToken(ch)
    if (t === undefined) return undefined
    const set = (this.foldAt(ch, t) as OrgFold).policyNow()
    return { seq: t.seq, set, has: new Set(set.map(toHex)), path: this.pathOf(ch, t) }
  }

  // ---- admission

  /**
   * Indexes an admitted output. The index is changed before the first
   * await; what follows reads back tokens whose predecessor is decided now
   * and deletes what was dropped from storage. An admission that meets a
   * restore in flight waits for it, so that it is decided as any other.
   */
  async outputAdmittedByTopic(payload: OutputAdmittedByTopic): Promise<void> {
    if (payload.mode !== 'whole-tx') return
    const hall = this.hallOf(payload.topic)
    if (hall === undefined) return
    let tx: Transaction
    try {
      tx = Transaction.fromAtomicBEEF(payload.atomicBEEF)
    } catch {
      this.host.log('ls_borg ignored an admission whose BEEF did not parse', { topic: payload.topic })
      return
    }
    while (this.inflight !== undefined) await this.inflight.catch(() => {})
    this.index(hall, tx, payload.outputIndex, 0)
    this.enforceTable()
    await this.drain()
    await this.flush()
  }

  /** Reads back, as the engine stores them, the tokens a full seq turned away whose predecessor has since been decided. */
  private async drain(): Promise<void> {
    const storage = this.storage
    if (storage === undefined) return
    while (this.reread.length > 0) {
      const r = this.reread.shift()!
      if (this.holdsToken(r.txid)) continue
      const o = await storage.findOutput(r.txid, r.vout, topic(r.hall), undefined, true)
      if (o?.beef === undefined) continue
      try {
        this.index(r.hall, Transaction.fromBEEF(o.beef, o.txid), r.vout, o.score ?? 0)
      } catch (e) {
        this.host.log('ls_borg could not read a token again', { txid: r.txid, err: String(e) })
      }
    }
  }

  outputEvicted(txid: string, outputIndex: number): void {
    this.forget(txid, outputIndex)
  }

  outputNoLongerRetainedInHistory(txid: string, outputIndex: number, t: string): void {
    const hall = this.hallOf(t)
    if (hall !== undefined) this.forget(txid, outputIndex, hall)
  }

  private hallOf(t: string): string | undefined {
    if (!t.startsWith(TopicPrefix)) return undefined
    const hall = t.slice(TopicPrefix.length)
    return this.halls.has(hall) ? hall : undefined
  }

  /**
   * Indexes output vout of an admitted transaction on hall, writing its rows
   * the first time it is seen. A carrier the rows say was dropped is not
   * indexed again, and its output is deleted. Returns whether it indexed.
   */
  private index(hall: string, tx: Transaction, vout: number, score: number): boolean {
    const out = tx.outputs[vout]
    if (out === undefined) return false
    // A held sweep whose tombstone this transaction spends leaves the unspent
    // rows storage hands a restore: the journal names it from now on.
    for (const i of tx.inputs) {
      const src = srcTxid(i)
      if (src === undefined || i.sourceOutputIndex !== 0) continue
      if (this.sweeps.get(src)?.halls.has(hall) === true && this.spentSweeps.get(src)?.has(hall) !== true) this.write({ k: 'w', txid: src, hall })
    }
    const script = Uint8Array.from(out.lockingScript.toBinary())
    const txid = tx.id('hex')
    const kind = classify(script)
    if (kind === 'grant' || kind === 'policy' || kind === 'wrap') return this.indexCarrier(hall, tx, kind, script)
    if (kind === 'org-token' || kind === 'group-token') return this.indexToken(hall, tx, vout)
    if (vout === 0 && isFundingShape(script)) return this.indexSweep(hall, tx, score)
    this.host.log('ls_borg indexed an output of no known kind', { hall, txid, vout })
    return false
  }

  private indexCarrier(hall: string, tx: Transaction, kind: 'grant' | 'policy' | 'wrap', script: Uint8Array): boolean {
    const input = tx.inputs[0]
    const src = input === undefined ? undefined : srcTxid(input)
    if (input === undefined || src === undefined) return false
    const op = outpoint(src, input.sourceOutputIndex)
    const txid = tx.id('hex')
    const c = toHex(Uint8Array.from(tx.hash() as number[]))
    const record = firstPush(script)!
    let e: CarrierEntry
    if (kind === 'grant') {
      const g = decodeGrant(record)
      e = { kind: 'g', c, txid, hall, org: toHex(g.org), op, subject: toHex(g.subject) }
    } else if (kind === 'policy') {
      const p = decodePolicy(record)
      e = { kind: 'p', c, txid, hall, org: toHex(p.org), op, role: p.role, to: p.to === null ? undefined : toHex(p.to) }
    } else {
      const w = decodeWrap(record)
      e = { kind: 'w', c, txid, hall, org: toHex(w.org), op, group: toHex(w.group), epoch: w.epoch, recipients: recipients(w).map(toHex) }
    }
    if (!this.lines.has(c)) {
      this.write({ k: 'c', op, c, txid, kind: e.kind, hall, at: this.now() })
      // Spends seen before any carrier used the outpoint join its row.
      const early = this.table.get(op)
      if (early !== undefined) {
        this.table.delete(op)
        for (const [s, sp] of early) {
          this.write({ k: 's', op, s, hall: sp.hall, h: sp.h, at: sp.at })
          this.host.metrics.inc('borg_retractions_total')
        }
      }
    }
    if (this.rows.get(op)!.dropped.has(c)) {
      this.queueDelete(txid, 0, hall)
      return false
    }
    this.carriers.set(c, e)
    this.carrierByTxid.set(txid, c)
    if (e.kind === 'g') {
      addTo(this.grantsBySubject, `${hall}|${e.org}|${e.subject}`, c)
      addTo(this.grantsByOrg, `${hall}|${e.org}`, c)
    } else if (e.kind === 'p') {
      addTo(this.policiesByOrg, `${hall}|${e.org}`, c)
      addTo(this.policiesByRole, `${hall}|${e.org}|${e.role}`, c)
      if (e.to !== undefined) addTo(this.policiesByTo, `${hall}|${e.org}|${e.to}`, c)
    } else {
      addTo(this.wrapsByEpoch, `${hall}|${e.org}|${e.group}|${e.epoch}`, c)
      for (const m of e.recipients!) addTo(this.wrapsByMember, `${hall}|${e.org}|${e.group}|${e.epoch}|${m}`, c)
    }
    this.enforceEvidence(op)
    return true
  }

  private indexToken(hall: string, tx: Transaction, vout: number): boolean {
    const t = readTokenOutput(tx.outputs[vout]!, vout)
    if (t === undefined) {
      this.host.log('ls_borg could not read a token', { hall, txid: tx.id('hex'), vout })
      return false
    }
    const rec = (t.org ?? t.group)!
    const txid = tx.id('hex')
    const org = toHex(rec.org)
    const group = t.group === undefined ? '' : toHex(t.group.group)
    const key = this.chainKey(hall, org, group)
    let ch = this.chains.get(key)
    if (ch === undefined) {
      ch = { kind: t.org !== undefined ? 'org' : 'group', hall, org, group, bySeq: new Map(), byTxid: new Map(), children: new Map(), status: new Map(), states: new Map(), turned: new Map() }
      this.chains.set(key, ch)
      if (group !== '') addTo(this.groupsByOrg, `${hall}|${org}`, group)
    }
    if (!this.tokenSeen.has(txid)) this.write({ k: 't', txid, vout, hall, at: this.now() })
    if (ch.byTxid.has(txid)) return true
    const { height, offset } = place(tx)
    const e: TokenEntry = {
      txid,
      vout,
      kind: ch.kind,
      hall,
      org,
      group,
      chain: key,
      seq: rec.seq,
      prev: rec.seq === 0 ? '' : displayTxid(rec.prev),
      rec,
      height,
      offset,
    }
    const list = [...(ch.bySeq.get(rec.seq) ?? []), e].sort((a, b) => cmp(a.txid, b.txid))
    ch.bySeq.set(rec.seq, list)
    ch.byTxid.set(txid, e)
    if (e.prev !== '') addTo(ch.children, e.prev, txid)
    if (this.restoring) return true
    this.refold(ch)
    // The seq's bound is applied with every status known, so the set kept
    // does not depend on arrival order.
    this.bound(ch, rec.seq)
    return ch.byTxid.has(txid)
  }

  private forgetToken(ch: ChainIdx, t: TokenEntry): void {
    ch.byTxid.delete(t.txid)
    const list = (ch.bySeq.get(t.seq) ?? []).filter((x) => x.txid !== t.txid)
    if (list.length === 0) ch.bySeq.delete(t.seq)
    else ch.bySeq.set(t.seq, list)
    if (t.prev !== '') removeFrom(ch.children, t.prev, t.txid)
    ch.status.clear()
    ch.states.clear()
  }

  private indexSweep(hall: string, tx: Transaction, score: number): boolean {
    const txid = tx.id('hex')
    let e = this.sweeps.get(txid)
    if (e === undefined) {
      const { height, offset } = place(tx)
      const spent: string[] = []
      for (const input of tx.inputs) {
        const src = srcTxid(input)
        if (src === undefined) continue
        const op = outpoint(src, input.sourceOutputIndex)
        spent.push(op)
        const r = this.rows.get(op)
        if (r !== undefined) {
          if (!r.sweeps.has(txid)) {
            this.write({ k: 's', op, s: txid, hall, h: height, at: this.now() })
            this.host.metrics.inc('borg_retractions_total')
          }
        } else {
          let m = this.table.get(op)
          if (m === undefined) this.table.set(op, (m = new Map()))
          if (!m.has(txid)) m.set(txid, { hall, h: height, at: this.now() })
        }
      }
      this.sweeps.set(txid, (e = { txid, halls: new Set(), spent, height, offset, score }))
    }
    // The same mined sweep admitted in another hall answers there too.
    if (!e.halls.has(hall)) {
      e.halls.add(hall)
      for (const op of e.spent) addTo(this.sweepsByOp, `${hall}|${op}`, txid)
    }
    return true
  }

  /** Whether a held sweep still records a spend: a row's, or one in the table. */
  private sweepLive(s: SweepEntry): boolean {
    return s.spent.some((op) => this.rows.get(op)?.sweeps.has(s.txid) === true || this.table.get(op)?.has(s.txid) === true)
  }

  /**
   * Keeps the table at its bound, oldest outpoint first. A sweep that then
   * records no spend at all is dropped and deleted from storage.
   */
  private enforceTable(): void {
    while (this.table.size > this.tableCap) {
      const [op, spends] = this.table.entries().next().value as [string, Map<string, Spend>]
      this.table.delete(op)
      for (const s of spends.keys()) {
        const e = this.sweeps.get(s)
        if (e === undefined || this.sweepLive(e)) continue
        for (const h of e.halls) this.queueDelete(s, 0, h)
        this.forget(s, 0)
        this.host.metrics.inc('borg_dropped_total', { why: 'table' })
      }
    }
  }

  /** Removes a held output from the index, in one hall or in all; its rows stay. */
  private forget(txid: string, vout: number, hall?: string): void {
    const c = vout === 0 ? this.carrierByTxid.get(txid) : undefined
    const e = c === undefined ? undefined : this.carriers.get(c)
    if (e !== undefined && (hall === undefined || e.hall === hall)) {
      this.carriers.delete(e.c)
      this.carrierByTxid.delete(txid)
      if (e.kind === 'g') {
        removeFrom(this.grantsBySubject, `${e.hall}|${e.org}|${e.subject}`, e.c)
        removeFrom(this.grantsByOrg, `${e.hall}|${e.org}`, e.c)
      } else if (e.kind === 'p') {
        removeFrom(this.policiesByOrg, `${e.hall}|${e.org}`, e.c)
        removeFrom(this.policiesByRole, `${e.hall}|${e.org}|${e.role}`, e.c)
        if (e.to !== undefined) removeFrom(this.policiesByTo, `${e.hall}|${e.org}|${e.to}`, e.c)
      } else {
        removeFrom(this.wrapsByEpoch, `${e.hall}|${e.org}|${e.group}|${e.epoch}`, e.c)
        for (const m of e.recipients!) removeFrom(this.wrapsByMember, `${e.hall}|${e.org}|${e.group}|${e.epoch}|${m}`, e.c)
      }
    }
    const s = this.sweeps.get(txid)
    if (s !== undefined && vout === 0) {
      for (const h of hall === undefined ? [...s.halls] : [hall]) {
        if (!s.halls.delete(h)) continue
        for (const op of s.spent) removeFrom(this.sweepsByOp, `${h}|${op}`, txid)
      }
      if (s.halls.size === 0) this.sweeps.delete(txid)
    }
    for (const ch of this.chains.values()) {
      if (hall !== undefined && ch.hall !== hall) continue
      const t = ch.byTxid.get(txid)
      if (t !== undefined && t.vout === vout) this.forgetToken(ch, t)
    }
  }

  // ---- drops and retention (spec section 11.3)

  /** Drops the carrier with commitment c: marks its row, takes it out of every answer, and queues its output for deletion. */
  private drop(c: string, why: string): void {
    const l = this.lines.get(c)!
    const r = this.rows.get(l.op)!
    if (r.dropped.has(c)) return
    this.write({ k: 'd', op: l.op, c, w: this.winnerOf(l.op) === c })
    this.forget(l.txid, 0)
    this.queueDelete(l.txid, 0, l.hall)
    this.host.metrics.inc('borg_dropped_total', { why })
    this.host.log('ls_borg dropped a carrier', { txid: l.txid, outpoint: l.op, why })
  }

  /**
   * Keeps at most 8 superseded wraps, or 64 superseded grants, on an
   * outpoint, and drops the rest, highest commitment first.
   */
  private enforceEvidence(op: string): void {
    const r = this.rows.get(op)!
    const kept = [...r.carriers.values()].filter((l) => !r.dropped.has(l.c) && this.status(l.c) === 'superseded')
    if (kept.length === 0) return
    const cap = kept[0]!.kind === 'w' ? EvidenceWraps : EvidenceGrants
    const order = kept.map((l) => l.c).sort(cmp)
    for (const c of order.slice(cap).reverse()) this.drop(c, 'evidence')
  }

  /**
   * Applies retention at the host's time and deletes what was dropped from
   * storage. Kept: tokens and matched sweeps for the host's life; a grant
   * while the answered roll holds it, one never in a roll until its outpoint
   * is swept, and otherwise for the retention period after it left the roll
   * or was retracted; the wraps of each group's current epoch, and those of
   * an earlier epoch for the retention period after the host first saw the
   * token that ended it. Returns how many carriers were dropped.
   */
  async prune(): Promise<number> {
    const now = this.now()
    const old = (from: number): boolean => now - from >= this.retention
    let n = 0
    const rolls = new Map<string, ReturnType<BorgLookupService['answerRoll']>>()
    const revokedAt = new Map<string, Map<string, number>>()
    const sets = new Map<string, ReturnType<BorgLookupService['answerPolicy']>>()
    const retiredAt = new Map<string, Map<string, number>>()
    for (const e of [...this.carriers.values()]) {
      const l = this.lines.get(e.c)!
      const r = this.rows.get(l.op)!
      const sweptAt = r.sweeps.size === 0 ? undefined : Math.min(...[...r.sweeps.values()].map((s) => s.at))
      let from: number | undefined
      if (e.kind === 'g') {
        const k = `${e.hall}|${e.org}`
        if (!rolls.has(k)) {
          const a = this.answerRoll(e.hall, e.org)
          rolls.set(k, a)
          const m = new Map<string, number>()
          // The path runs from the answered token down, so the first revocation met is the latest.
          for (const t of a?.path ?? []) {
            for (const c of (t.rec as Org).revoke) if (!m.has(toHex(c))) m.set(toHex(c), this.tokenSeen.get(t.txid) ?? now)
          }
          revokedAt.set(k, m)
        }
        if (rolls.get(k)?.set.has(e.c) === true) continue
        const left = revokedAt.get(k)!.get(e.c)
        if (sweptAt === undefined && left === undefined) continue
        from = Math.max(l.at, sweptAt ?? 0, left ?? 0)
      } else if (e.kind === 'p') {
        // A policy record is kept while the set holds it, and for the floor
        // after it leaves the set or is retracted: a service answering for a
        // decision needs the record the decision rested on.
        const k = `${e.hall}|${e.org}`
        if (!sets.has(k)) {
          const a = this.answerPolicy(e.hall, e.org)
          sets.set(k, a)
          const m = new Map<string, number>()
          for (const t of a?.path ?? []) {
            for (const c of (t.rec as Org).retire) if (!m.has(toHex(c))) m.set(toHex(c), this.tokenSeen.get(t.txid) ?? now)
          }
          retiredAt.set(k, m)
        }
        if (sets.get(k)?.has.has(e.c) === true) continue
        const left = retiredAt.get(k)!.get(e.c)
        if (sweptAt === undefined && left === undefined) continue
        from = Math.max(l.at, sweptAt ?? 0, left ?? 0)
      } else {
        const ch = this.chains.get(this.chainKey(e.hall, e.org, e.group!))
        if (sweptAt !== undefined) {
          from = Math.max(l.at, sweptAt)
        } else if (ch === undefined) {
          from = l.at
        } else {
          let latest = -1
          let ended: number | undefined
          for (const t of ch.byTxid.values()) {
            const ep = (t.rec as Group).epoch
            latest = Math.max(latest, ep)
            if (ep > e.epoch!) {
              const seen = this.tokenSeen.get(t.txid) ?? now
              ended = ended === undefined ? seen : Math.min(ended, seen)
            }
          }
          if (e.epoch! >= latest || ended === undefined) continue
          from = Math.max(l.at, ended)
        }
      }
      if (old(from)) {
        this.drop(e.c, 'retention')
        n++
      }
    }
    await this.flush()
    return n
  }

  /**
   * Deletes dropped outputs from storage. It runs after every admission and
   * after retention, so a dropped output is gone at once. A store that
   * cannot delete keeps what was dropped; the queue is not kept for it.
   */
  async flush(): Promise<void> {
    if (this.storage === undefined || this.pending.size === 0) return
    const del = this.storage.deleteOutput?.bind(this.storage)
    if (del === undefined) {
      if (!this.warnedNoDelete) this.host.log('ls_borg cannot delete dropped outputs: the storage has no deleteOutput', {})
      this.warnedNoDelete = true
      this.pending.clear()
      return
    }
    for (const [k, p] of [...this.pending]) {
      try {
        await del(p.txid, p.vout, p.topic)
        this.pending.delete(k)
      } catch (e) {
        this.host.log('ls_borg could not delete a dropped output', { txid: p.txid, err: String(e) })
      }
    }
  }

  // ---- questions (spec section 10.2)

  /** Asked through the engine: a class the host prices is refused here. */
  async lookup(question: LookupQuestion): Promise<LookupFormula> {
    return await this.answer(question, false)
  }

  /** The class of a question, validated, and whether this host prices it. */
  classify(question: LookupQuestion): { q: Question; priced: boolean } {
    if (question.service !== ServiceName) throw new Error(`ls_borg: this service is ${ServiceName}, not ${String(question.service)}`)
    let q: Question
    try {
      q = parseQuery(question.query)
    } catch (e) {
      if (e instanceof Refusal) throw new Error(`ls_borg: refused: ${e.message.replace(/^borgrec: /, '')}`)
      throw e
    }
    if (!this.halls.has(q.members['hall']!)) throw new Error(`ls_borg: this host does not carry hall ${q.members['hall']}`)
    return { q, priced: !q.class.free && this.priced.has(q.class.name) }
  }

  /**
   * Answers a question. paid says the question was paid for (or asked where
   * no payment is due); a class this host prices is refused otherwise.
   */
  async answer(question: LookupQuestion, paid: boolean): Promise<LookupFormula> {
    const { q, priced } = this.classify(question)
    if (priced && !paid) throw new Error(`ls_borg: ${q.class.name} is priced on this host; ask its terms route's /lookup over BRC-104`)
    this.host.metrics.inc('borg_lookups_total', { class: q.class.name })
    const m = q.members
    const hall = m['hall']!
    const limit = q.class.limit
    const tok = (t: TokenEntry): LookupFormula[number] => ({ txid: t.txid, outputIndex: t.vout })
    const car = (e: CarrierEntry): LookupFormula[number] => ({ txid: e.txid, outputIndex: 0 })
    const chain = (org: string, group: string): ChainIdx | undefined => this.chains.get(this.chainKey(hall, org, group))
    const atSeq = (ch: ChainIdx | undefined, seq: number): TokenEntry[] => [...(ch?.bySeq.get(seq) ?? [])]
    const latest = (ch: ChainIdx | undefined): TokenEntry[] => {
      if (ch === undefined || ch.bySeq.size === 0) return []
      return atSeq(ch, Math.max(...ch.bySeq.keys()))
    }
    const held = (cs: Iterable<string> | undefined): CarrierEntry[] =>
      [...(cs ?? [])]
        .map((c) => this.carriers.get(c)!)
        .filter((e) => this.status(e.c) === 'held')
        .sort((a, b) => cmp(a.txid, b.txid))
    switch (q.class.name) {
      case 'org':
        return latest(chain(m['org']!, '')).slice(0, limit).map(tok)
      case 'org-seq':
        return atSeq(chain(m['org']!, ''), Number(m['seq'])).slice(0, limit).map(tok)
      case 'group':
        return latest(chain(m['org']!, m['group']!)).slice(0, limit).map(tok)
      case 'group-seq':
        return atSeq(chain(m['org']!, m['group']!), Number(m['seq'])).slice(0, limit).map(tok)
      case 'roll':
      case 'roll-after': {
        const a = this.answerRoll(hall, m['roll']!)
        if (a === undefined) return []
        const after = m['after']
        return [...(this.grantsByOrg.get(`${hall}|${m['roll']}`) ?? [])]
          .map((c) => this.carriers.get(c)!)
          .filter((e) => a.set.has(e.c) && (after === undefined || e.txid > after))
          .sort((x, y) => cmp(x.txid, y.txid))
          .slice(0, limit)
          .map(car)
      }
      case 'member': {
        const a = this.answerRoll(hall, m['org']!)
        if (a === undefined) return []
        return [...(this.grantsBySubject.get(`${hall}|${m['org']}|${m['member']}`) ?? [])]
          .map((c) => this.carriers.get(c)!)
          .filter((e) => a.set.has(e.c))
          .sort((x, y) => cmp(x.txid, y.txid))
          .slice(0, limit)
          .map((e) => ({ ...car(e), context: Array.from(encodeContext(proveGrant(a.seq, a.roll, fromHex(e.c)))) }))
      }
      case 'policy':
      case 'policy-after': {
        const a = this.answerPolicy(hall, m['policy']!)
        if (a === undefined) return []
        const after = m['after']
        return [...(this.policiesByOrg.get(`${hall}|${m['policy']}`) ?? [])]
          .map((c) => this.carriers.get(c)!)
          .filter((e) => a.has.has(e.c) && (after === undefined || e.txid > after))
          .sort((x, y) => cmp(x.txid, y.txid))
          .slice(0, limit)
          .map(car)
      }
      case 'role':
      case 'role-after':
      case 'bound':
      case 'bound-after': {
        const a = this.answerPolicy(hall, m['org']!)
        if (a === undefined) return []
        const after = m['after']
        const from =
          q.class.name.startsWith('role')
            ? this.policiesByRole.get(`${hall}|${m['org']}|${m['role']}`)
            : this.policiesByTo.get(`${hall}|${m['org']}|${m['boundTo']}`)
        return [...(from ?? [])]
          .map((c) => this.carriers.get(c)!)
          .filter((e) => a.has.has(e.c) && (after === undefined || e.txid > after))
          .sort((x, y) => cmp(x.txid, y.txid))
          .slice(0, limit)
          .map((e) => ({ ...car(e), context: Array.from(encodeContext(provePolicy(a.seq, a.set, fromHex(e.c)))) }))
      }
      case 'groups':
      case 'groups-after': {
        const out: LookupFormula = []
        for (const g of this.groupsOf(hall, m['groupsOf']!, m['after'])) {
          const ts = latest(chain(m['groupsOf']!, g))
          if (out.length > 0 && out.length + ts.length > limit) break
          out.push(...ts.map(tok))
        }
        return out.slice(0, limit)
      }
      case 'member-of':
      case 'member-of-after': {
        const out: LookupFormula = []
        for (const g of this.groupsOf(hall, m['org']!, m['after'])) {
          const ch = chain(m['org']!, g)!
          const t = this.answerToken(ch)
          if (t === undefined || !(this.foldAt(ch, t) as GroupFold).holds(m['memberOf']!)) continue
          out.push(tok(t))
          if (out.length >= limit) break
        }
        return out
      }
      case 'wraps':
      case 'wraps-after': {
        const after = m['after']
        return held(this.wrapsByEpoch.get(`${hall}|${m['org']}|${m['group']}|${m['epoch']}`))
          .filter((e) => after === undefined || e.txid > after)
          .slice(0, limit)
          .map(car)
      }
      case 'wrap':
        return held(this.wrapsByMember.get(`${hall}|${m['org']}|${m['group']}|${m['epoch']}|${m['member']}`)).slice(0, limit).map(car)
      case 'sweep':
        return [...(this.sweepsByOp.get(`${hall}|${m['spent']}`) ?? [])]
          .map((s) => this.sweeps.get(s)!)
          .sort((a, b) => a.height - b.height || a.offset - b.offset || cmp(a.txid, b.txid))
          .slice(0, limit)
          .map((s) => ({ txid: s.txid, outputIndex: 0 }))
      case 'history':
      case 'history-after': {
        const ch = chain(m['history']!, m['chain'] === 'org' ? '' : m['chain']!)
        if (ch === undefined) return []
        const after = m['after'] === undefined ? -1 : Number(m['after'])
        const out: LookupFormula = []
        for (const seq of [...ch.bySeq.keys()].filter((s) => s > after).sort((a, b) => a - b)) {
          const ts = atSeq(ch, seq)
          // A page never splits a seq, so the cursor loses nothing.
          if (out.length > 0 && out.length + ts.length > limit) break
          out.push(...ts.map(tok))
        }
        return out
      }
    }
    throw new Error(`ls_borg: no answer for class ${q.class.name}`)
  }

  /** The group ids of an organisation in a hall, ascending, strictly after after. */
  private groupsOf(hall: string, org: string, after: string | undefined): string[] {
    return [...(this.groupsByOrg.get(`${hall}|${org}`) ?? [])].filter((g) => after === undefined || g > after).sort(cmp)
  }

  /**
   * An answer with each output's BEEF, as the engine hydrates one, read from
   * the host's storage: for the terms route, which answers outside the
   * engine. A context rides along as the engine passes it.
   */
  async hydrate(formula: LookupFormula): Promise<OutputList> {
    const storage = this.storage
    if (storage === undefined) throw new Error('ls_borg: storage is not restored yet')
    const outputs: OutputList['outputs'] = []
    for (const { txid, outputIndex, context } of formula) {
      const o = await storage.findOutput(txid, outputIndex, undefined, undefined, true)
      if (o?.beef === undefined) continue
      outputs.push(context === undefined ? { beef: o.beef, outputIndex } : { beef: o.beef, outputIndex, context })
    }
    return { type: 'output-list', outputs }
  }

  get restored(): boolean {
    return this.storage !== undefined
  }

  // ---- restore (spec section 11.4)

  /**
   * Rebuilds the index: the rows from the journal, then every output of this
   * module's topics the host hands over (a row handed without its BEEF is
   * read again from storage with it), then every token, carrier and sweep the
   * journal names that the unspent rows did not carry (a token its successor
   * spent, say), and then each held token's predecessors, read through the
   * input that spends them from the token as the engine stores it (the token
   * alone, without its parent). A held sweep whose tombstone was spent is
 * among those the journal names (its `w` line). Sweeps are taken in storage's order, so the
   * bounded table keeps the newest. Statuses are functions of what is held,
   * so the order storage returns rows in changes nothing else. A storage
   * failure propagates: a restore that skipped a sweep would answer
   * retracted carriers.
   */
  async restore(outputs: Output[], storage: RestoreStorage): Promise<number> {
    while (this.inflight !== undefined) await this.inflight.catch(() => {})
    // Marked in flight before any of it runs, so nothing is admitted into a half-cleared index.
    this.inflight = Promise.resolve().then(async () => await this.restoreAll(outputs, storage))
    try {
      return await this.inflight
    } finally {
      this.inflight = undefined
    }
  }

  private async restoreAll(outputs: Output[], storage: RestoreStorage): Promise<number> {
    for (const m of [
      this.rows,
      this.lines,
      this.tokenSeen,
      this.chains,
      this.carriers,
      this.carrierByTxid,
      this.grantsBySubject,
      this.grantsByOrg,
      this.wrapsByEpoch,
      this.wrapsByMember,
      this.groupsByOrg,
      this.sweeps,
      this.sweepsByOp,
      this.table,
      this.pending,
      this.spentSweeps,
    ]) {
      m.clear()
    }
    this.reread.length = 0
    const journal = this.journal.load()
    for (const l of journal) this.apply(l)
    const full = storage as Storage
    this.storage = full
    const seen = new Set<string>()
    const queue: Output[] = []
    for (const o of outputs) {
      const k = `${o.topic}|${o.txid}.${o.outputIndex}`
      if (this.hallOf(o.topic) === undefined || seen.has(k)) continue
      seen.add(k)
      queue.push(o)
    }
    const named: Array<{ topic: string; txid: string; vout: number }> = []
    for (const l of journal) {
      if (l.k === 'c' || l.k === 's' || l.k === 't' || l.k === 'w') {
        const hall = l.hall
        if (!this.halls.has(hall)) continue
        if (l.k === 'c' || l.k === 'w') named.push({ topic: topic(hall), txid: l.txid, vout: 0 })
        else if (l.k === 's') named.push({ topic: topic(hall), txid: l.s, vout: 0 })
        else named.push({ topic: topic(hall), txid: l.txid, vout: l.vout })
      }
    }
    for (const n of named) {
      const k = `${n.topic}|${n.txid}.${n.vout}`
      if (seen.has(k)) continue
      seen.add(k)
      const o = await full.findOutput(n.txid, n.vout, n.topic, undefined, true)
      if (o !== null) queue.push(o)
    }
    const read = async (o: Output): Promise<Transaction | undefined> => {
      let row: Output | null = o
      if (row.beef === undefined) row = (await full.findOutput(o.txid, o.outputIndex, o.topic, undefined, true)) ?? row
      try {
        return row.beef === undefined ? undefined : Transaction.fromBEEF(row.beef, row.txid)
      } catch {
        return undefined
      }
    }
    const txs: Array<{ o: Output; tx: Transaction; sweep: boolean }> = []
    for (const o of queue) {
      const tx = await read(o)
      if (tx === undefined) {
        this.host.log('ls_borg could not restore an output without its transaction', { topic: o.topic, txid: o.txid })
        continue
      }
      const out = tx.outputs[o.outputIndex]
      const sweep = out !== undefined && o.outputIndex === 0 && isFundingShape(Uint8Array.from(out.lockingScript.toBinary()))
      txs.push({ o, tx, sweep })
    }
    // Carriers and tokens first, sweeps after them in storage's order.
    txs.sort((a, b) => Number(a.sweep) - Number(b.sweep) || (a.sweep ? (a.o.score ?? 0) - (b.o.score ?? 0) || cmp(a.o.txid, b.o.txid) : 0))
    let indexed = 0
    this.restoring = true
    const take = (o: Output, tx: Transaction): void => {
      try {
        if (this.index(this.hallOf(o.topic)!, tx, o.outputIndex, o.score ?? 0)) indexed++
      } catch (e) {
        this.host.log('ls_borg could not restore an output', { topic: o.topic, txid: o.txid, err: String(e) })
      }
    }
    for (const { o, tx } of txs) take(o, tx)
    // Each held token's predecessors, through the input that spends them.
    const walk: Array<{ hall: string; tx: Transaction; prev: string }> = []
    for (const { o, tx } of txs) {
      const t = this.tokenAt(o.txid, o.outputIndex)
      if (t !== undefined && t.prev !== '') walk.push({ hall: t.hall, tx, prev: t.prev })
    }
    while (walk.length > 0) {
      const w = walk.pop()!
      if (this.holdsToken(w.prev)) continue
      const input = w.tx.inputs.find((i) => srcTxid(i) === w.prev)
      if (input === undefined) continue
      const o = await full.findOutput(w.prev, input.sourceOutputIndex, topic(w.hall), undefined, true)
      if (o === null) continue
      const tx = await read(o)
      if (tx === undefined) continue
      take(o, tx)
      const t = this.tokenAt(o.txid, o.outputIndex)
      if (t !== undefined && t.prev !== '') walk.push({ hall: t.hall, tx, prev: t.prev })
    }
    this.restoring = false
    // Each seq's bound is applied once, here, when every token's status is known.
    for (const ch of this.chains.values()) {
      this.refold(ch)
      for (const seq of [...ch.bySeq.keys()].sort((a, b) => a - b)) this.bound(ch, seq)
    }
    this.enforceTable()
    await this.drain()
    await this.flush()
    return indexed
  }

  private holdsToken(txid: string): boolean {
    for (const ch of this.chains.values()) if (ch.byTxid.has(txid)) return true
    return false
  }

  private tokenAt(txid: string, vout: number): TokenEntry | undefined {
    for (const ch of this.chains.values()) {
      const t = ch.byTxid.get(txid)
      if (t !== undefined && t.vout === vout) return t
    }
    return undefined
  }

  async getDocumentation(): Promise<string> {
    return [
      '# ls_borg',
      '',
      'Answers every hall this host carries (spec section 10.2). Free classes: `org`,',
      '`org-seq`, `group`, `group-seq` (a chain\'s latest tokens or those at one seq, at',
      'most 8), `roll`, `roll-after` (the grant carriers of an organisation\'s roll, 64 a',
      'page), `member` (a key\'s grants in the roll, each with its inclusion path as',
      'context), `groups`, `groups-after`, `member-of`, `member-of-after` (the latest',
      'token of each group), `wraps`, `wraps-after`, `wrap` (an epoch\'s wrap carriers)',
      'and `sweep` (sweeps spending an outpoint). Priceable: `history`, `history-after`',
      '(every token of a chain, by seq). A question must carry exactly the members of',
      'one class; anything else is refused with an error. An empty answer means only',
      'that this host holds nothing that answers.',
    ].join('\n')
  }

  async getMetaData(): Promise<LookupServiceMetaData> {
    return { name: ServiceName, shortDescription: 'Answers borg organisations, groups, grants, wraps and sweeps by hall.' }
  }
}
