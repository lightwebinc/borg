/**
 * Test helpers that make fresh borg objects the golden vectors do not hold:
 * funding trees and chain tokens mined on a local chain, grants issued and
 * wraps sealed by the TypeScript SDK, and sweeps. OrgSim is a minimal
 * stand-in for an organisation's tooling: it keeps the roll, each group's
 * members and epoch key, and builds every transition the way spec section
 * 7.1 says, or, on request, the way it says not to. Every key is drawn from
 * a fixed label, so a party is the same on every run. Nothing here is part
 * of the module the host loads.
 */
import { createCipheriv, randomBytes } from 'node:crypto'
import {
  Certificate,
  CompletedProtoWallet,
  Hash,
  KeyDeriver,
  LockingScript,
  MerklePath,
  P2PKH,
  PrivateKey,
  ProtoWallet,
  PushDrop,
  Transaction,
  UnlockingScript,
  type ChainTracker,
  type CreateActionArgs,
  type CreateActionResult,
} from '@bsv/sdk'
import { LockTime, compareBytes, readerLockingKey } from '@lightwebinc/bcommon'
import {
  KeyChain,
  KeyGrant,
  KeyGroup,
  KeyOrg,
  KeyRecord,
  Protocol,
  ReleaseProtocol,
  TagGroup,
  TagOrg,
  WrapVersion,
  encodeGrant,
  encodeGroup,
  encodeOrg,
  encodeWrap,
  grantTypeBase64,
  releaseKeyID,
  type Grant,
  type Group,
  type Org,
} from './borgrec.js'
import { tokenBEEF } from './beef.js'
import { commitmentOf, epochID, releaseKeyIDString } from './epoch.js'
import { membersRoot, rollRoot } from './merkle.js'
import { fundingScript, pushDropScript } from './script.js'
import { concat, displayTxid, fromHex, toBase64, toHex } from './util.js'

const text = new TextEncoder()
export const sha = (s: string): Uint8Array => Uint8Array.from(Hash.sha256(Array.from(text.encode(s))))
const ls = (b: Uint8Array): LockingScript => LockingScript.fromBinary(Array.from(b))

/** A local chain: blocks of two, each a minted transaction beside a fixed sibling. */
export class Chain {
  readonly roots = new Map<number, string>()
  private height: number

  constructor(first = 50000, extra: Array<{ height: number; merkleRoot: string }> = []) {
    this.height = first
    for (const h of extra) this.roots.set(h.height, h.merkleRoot)
  }

  /** Gives tx a proof in a new block and returns it. */
  mine(tx: Transaction): Transaction {
    const height = this.height++
    tx.merklePath = new MerklePath(height, [
      [
        { offset: 0, hash: '55'.repeat(32) },
        { offset: 1, hash: tx.id('hex'), txid: true },
      ],
    ])
    this.roots.set(height, tx.merklePath.computeRoot(tx.id('hex')))
    return tx
  }

  /** a and b in one new block of four, at offsets 1 and 2, each with its own minimal path. */
  mineTogether(a: Transaction, b: Transaction): void {
    const height = this.height++
    const s0 = '66'.repeat(32)
    const s3 = '77'.repeat(32)
    const pair = (l: string, r: string): string => toHex(Uint8Array.from(Hash.hash256(Array.from(concat([fromHex(l).reverse(), fromHex(r).reverse()])))).reverse())
    a.merklePath = new MerklePath(height, [
      [
        { offset: 0, hash: s0 },
        { offset: 1, hash: a.id('hex'), txid: true },
      ],
      [{ offset: 1, hash: pair(b.id('hex'), s3) }],
    ])
    b.merklePath = new MerklePath(height, [
      [
        { offset: 2, hash: b.id('hex'), txid: true },
        { offset: 3, hash: s3 },
      ],
      [{ offset: 0, hash: pair(s0, a.id('hex')) }],
    ])
    this.roots.set(height, a.merklePath.computeRoot(a.id('hex')))
  }

  get tracker(): ChainTracker {
    return {
      isValidRootForHeight: async (root, height) => this.roots.get(height) === root,
      currentHeight: async () => Math.max(...this.roots.keys()),
    }
  }

  get headers(): Array<{ height: number; merkleRoot: string }> {
    return [...this.roots].map(([height, merkleRoot]) => ({ height, merkleRoot }))
  }
}

/** One identity: its key, its wallet and its compressed public key. */
export class Party {
  readonly key: PrivateKey
  readonly id: Uint8Array
  readonly hex: string
  readonly wallet: CompletedProtoWallet

  constructor(
    readonly label: string,
    key?: PrivateKey,
  ) {
    this.key = key ?? PrivateKey.fromHex(toHex(sha(`borg/test/party/${label}`)))
    this.id = Uint8Array.from(this.key.toPublicKey().encode(true) as number[])
    this.hex = toHex(this.id)
    this.wallet = new CompletedProtoWallet(this.key)
  }

  /** The key a reader derives for this party's outputs under keyID. */
  derived(keyID: string): Uint8Array {
    return Uint8Array.from(readerLockingKey(Protocol, keyID, this.hex).encode(true) as number[])
  }

  async sign(keyID: string, data: Uint8Array): Promise<Uint8Array> {
    const r = await this.wallet.createSignature({ data: Array.from(data), protocolID: Protocol, keyID, counterparty: 'anyone' })
    return Uint8Array.from(r.signature)
  }

  unlock(keyID: string): ReturnType<PushDrop['unlock']> {
    return new PushDrop(this.wallet).unlock(Protocol, keyID, 'anyone', 'all', false)
  }
}

let nowhere = 0

/** A transaction from nowhere: one unsigned input of an outpoint no one holds. */
export function fromNowhere(outs: Array<{ satoshis: number; lockingScript: LockingScript }>): Transaction {
  return new Transaction(
    1,
    [{ sourceTXID: toHex(sha(`borg/test/nowhere/${nowhere++}/${Date.now()}`)), sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff }],
    outs,
    0,
  )
}

/** A mined funding tree of n outputs of 1000 satoshis, locked to derive(owner, keyID). */
export function fundingTree(owner: Party, keyID: string, n: number, chain: Chain): Transaction {
  const lock = ls(fundingScript(owner.derived(keyID)))
  return chain.mine(fromNowhere(Array.from({ length: n }, () => ({ satoshis: 1000, lockingScript: lock }))))
}

/** A carrier of record spending output vout of owner's tree, locked and signed under keyID. */
export async function carrier(tree: Transaction, vout: number, owner: Party, keyID: string, record: Uint8Array, lockTime = LockTime): Promise<Transaction> {
  const lock = pushDropScript(owner.derived(keyID), [record], await owner.sign(keyID, record))
  const tx = new Transaction(
    1,
    [{ sourceTransaction: tree, sourceOutputIndex: vout, unlockingScriptTemplate: owner.unlock(keyID), sequence: 0 }],
    [{ satoshis: tree.outputs[vout]!.satoshis!, lockingScript: ls(lock) }],
    lockTime,
  )
  await tx.sign()
  return tx
}

/**
 * An unmined chain token: output 0 the signed two-field PushDrop of tag and
 * record under keyID, 1 satoshi; each input spends one of inputs with the
 * owner's key under its keyID; what the inputs hold beyond 1 satoshi goes
 * to change.
 */
export async function token(owner: Party, tag: Uint8Array, record: Uint8Array, keyID: string, inputs: Array<{ src: Transaction; vout: number; keyID: string }>): Promise<Transaction> {
  const lock = pushDropScript(owner.derived(keyID), [tag, record], await owner.sign(keyID, concat([tag, record])))
  const total = inputs.reduce((n, i) => n + i.src.outputs[i.vout]!.satoshis!, 0)
  const outs = [{ satoshis: 1, lockingScript: ls(lock) }]
  if (total > 1) outs.push({ satoshis: total - 1, lockingScript: new P2PKH().lock(owner.key.toAddress()) })
  const tx = new Transaction(
    1,
    inputs.map((i) => ({ sourceTransaction: i.src, sourceOutputIndex: i.vout, unlockingScriptTemplate: owner.unlock(i.keyID), sequence: 0xffffffff })),
    outs,
    0,
  )
  await tx.sign()
  return tx
}

/** A mined sweep of outputs vouts of owner's tree under keyID: its tombstone, funding-shaped, at output 0. */
export async function sweep(tree: Transaction, vouts: number[], owner: Party, keyID: string, chain: Chain): Promise<Transaction> {
  const value = vouts.reduce((n, v) => n + tree.outputs[v]!.satoshis!, 0)
  const tx = new Transaction(
    1,
    vouts.map((v) => ({ sourceTransaction: tree, sourceOutputIndex: v, unlockingScriptTemplate: owner.unlock(keyID), sequence: 0xffffffff })),
    [{ satoshis: value, lockingScript: ls(fundingScript(owner.derived(keyID))) }],
    0,
  )
  await tx.sign()
  return chain.mine(tx)
}

/** C = txid(K), hash byte order. */
export const commitment = (tx: Transaction): Uint8Array => Uint8Array.from(tx.hash() as number[])
export const cHex = (tx: Transaction): string => toHex(commitment(tx))

/** AES-256-GCM with a 32-byte IV: BRC-2's symmetric form. */
function seal(key: Uint8Array, plaintext: Uint8Array): Uint8Array {
  const iv = Uint8Array.from(randomBytes(32))
  const c = createCipheriv('aes-256-gcm', key, iv)
  return concat([iv, c.update(plaintext), c.final(), c.getAuthTag()])
}

export interface GrantValues {
  expiry: string
  role: string
  scope: string
}

export const DefaultValues: GrantValues = { expiry: '2099-12-31T23:59:59Z', role: 'member', scope: 'member' }

/** A grant from org to subject whose revocation outpoint is tree:vout, issued and signed by the SDK. */
export async function issueGrant(hall: string, org: Party, subject: Party, tree: Transaction, vout: number, values: GrantValues = DefaultValues, reveal = true): Promise<Grant> {
  const fields = new Map<string, Uint8Array>()
  const keyring = new Map<string, Uint8Array>()
  const rev = new Map<string, Uint8Array>()
  const deriver = new KeyDeriver(org.key)
  for (const f of ['expiry', 'role', 'scope'] as const) {
    const fk = Uint8Array.from(randomBytes(32))
    fields.set(f, seal(fk, text.encode(values[f])))
    const sk = deriver.deriveSymmetricKey([2, 'certificate field encryption'], f, subject.hex)
    keyring.set(f, seal(Uint8Array.from(sk.toArray('be', 32)), fk))
    rev.set(f, fk)
  }
  const revTxid = Uint8Array.from(tree.hash() as number[])
  const g: Grant = {
    hall,
    org: org.id,
    subject: subject.id,
    serial: Uint8Array.from(randomBytes(32)),
    revTxid,
    revVout: vout,
    fields,
    signature: new Uint8Array(0),
    keyring,
    reveal: reveal ? rev : undefined,
    extra: [],
  }
  const b64: Record<string, string> = {}
  for (const [f, c] of fields) b64[f] = toBase64(c)
  const cert = new Certificate(grantTypeBase64(), toBase64(g.serial), subject.hex, org.hex, `${displayTxid(revTxid)}.${vout}`, b64)
  await cert.sign(org.wallet)
  g.signature = fromHex(cert.signature as string)
  return g
}

/** One release of k to member, sealed by org (spec section 6.3). */
export function wrapEntry(org: Party, member: Uint8Array, epochId: Uint8Array, epoch: number, k: Uint8Array): Uint8Array {
  const sk = new KeyDeriver(org.key).deriveSymmetricKey(ReleaseProtocol, releaseKeyIDString(epochId, epoch), toHex(member))
  return concat([WrapVersion, org.id, member, releaseKeyID(epochId, epoch), seal(Uint8Array.from(sk.toArray('be', 32)), k)])
}

/** A fresh epoch key: a scalar in [1, n-1]. */
export const epochKey = (): Uint8Array => Uint8Array.from(PrivateKey.fromRandom().toArray('be', 32))

const t0 = 1798761600
const sortKeys = (keys: Uint8Array[]): Uint8Array[] => [...keys].sort(compareBytes)
const hasKey = (keys: Uint8Array[], k: Uint8Array): boolean => keys.some((x) => compareBytes(x, k) === 0)

/** A mined token and the BEEF it is published in. */
export interface Minted<R> {
  tx: Transaction
  beef: Uint8Array
  rec: R
  record: Uint8Array
}

/**
 * A minimal organisation's tooling over a local chain: its trees, its roll,
 * its grants and its groups. Each token is mined in a block of its own and
 * published with its parent (tokenBEEF).
 */
export class OrgSim {
  readonly party: Party
  readonly grantTree: Transaction
  readonly recordTree: Transaction
  readonly chainTree: Transaction
  private grantAt = 0
  private recordAt = 0
  private chainAt = 0
  tip: Minted<Org> | undefined
  /** Every organisation token minted, seq order. */
  readonly history: Array<Minted<Org>> = []
  roll: Uint8Array[] = []
  readonly groups = new Map<string, GroupSim>()

  constructor(
    readonly hall: string,
    label: string,
    readonly chain: Chain,
    readonly opts: { opaque?: boolean; trees?: number; key?: PrivateKey } = {},
  ) {
    this.party = new Party(label, opts.key)
    const n = opts.trees ?? 32
    this.grantTree = fundingTree(this.party, KeyGrant, n, chain)
    this.recordTree = fundingTree(this.party, KeyRecord, n, chain)
    this.chainTree = fundingTree(this.party, KeyChain, 8, chain)
  }

  get hex(): string {
    return this.party.hex
  }

  /** The next grant-tree output, and the next record-tree output. */
  nextGrantVout(): number {
    return this.grantAt++
  }
  nextRecordVout(): number {
    return this.recordAt++
  }
  nextChainVout(): number {
    return this.chainAt++
  }

  /** A grant carrier to subject on the next grant-tree output (or vout). */
  async grant(subject: Party, values: GrantValues = DefaultValues, o: { vout?: number; lockTime?: number } = {}): Promise<{ tx: Transaction; c: Uint8Array; grant: Grant; vout: number }> {
    const vout = o.vout ?? this.nextGrantVout()
    const g = await issueGrant(this.hall, this.party, subject, this.grantTree, vout, values, this.opts.opaque !== true)
    const tx = await carrier(this.grantTree, vout, this.party, KeyGrant, encodeGrant(g), o.lockTime ?? LockTime)
    return { tx, c: commitment(tx), grant: g, vout }
  }

  /**
   * The next organisation token, admitting and revoking commitments, mined.
   * mutate may change the record before it is encoded (to build what a host
   * must refuse or the index must flag); the roll kept here follows the
   * change lists as given.
   */
  async orgToken(admit: Uint8Array[], revoke: Uint8Array[] = [], o: { mutate?: (r: Org) => void; mine?: boolean } = {}): Promise<Minted<Org>> {
    const roll = sortKeys([...this.roll.filter((c) => !hasKey(revoke, c)), ...admit])
    const seq = this.tip === undefined ? 0 : this.tip.rec.seq + 1
    const rec: Org = {
      hall: this.hall,
      org: this.party.id,
      seq,
      prev: this.tip === undefined ? new Uint8Array(32) : commitment(this.tip.tx),
      name: 'Example Organisation',
      admit: sortKeys(admit),
      revoke: sortKeys(revoke),
      count: roll.length,
      roll: rollRoot(roll),
      opaque: this.opts.opaque === true,
      created: t0 + seq * 600,
      extra: [],
    }
    o.mutate?.(rec)
    const record = encodeOrg(rec)
    const input = this.tip === undefined ? { src: this.chainTree, vout: this.nextChainVout(), keyID: KeyChain } : { src: this.tip.tx, vout: 0, keyID: KeyOrg }
    const tx = await token(this.party, TagOrg, record, KeyOrg, [input])
    if (o.mine !== false) this.chain.mine(tx)
    const m: Minted<Org> = { tx, beef: o.mine === false ? new Uint8Array(0) : tokenBEEF(tx, input.src), rec, record }
    this.tip = m
    this.history.push(m)
    this.roll = roll
    return m
  }

  /** A new group of members at epoch 0, mined, with its id drawn at random. */
  async group(name: string, members: Party[], o: { rotateOnJoin?: boolean; id?: Uint8Array } = {}): Promise<GroupSim> {
    const g = new GroupSim(this, name, o.id ?? Uint8Array.from(randomBytes(16)), o.rotateOnJoin === true)
    await g.create(members)
    this.groups.set(toHex(g.id), g)
    return g
  }
}

/** One group of an OrgSim: its chain, members, the keys removed from it, and every epoch's key. */
export class GroupSim {
  tip!: Minted<Group>
  /** Every token minted, seq order. */
  readonly history: Array<Minted<Group>> = []
  members: Uint8Array[] = []
  readonly removed: Uint8Array[] = []
  /** Epoch number to its key and its epoch id. */
  readonly epochs = new Map<number, { key: Uint8Array; epochId: Uint8Array }>()

  constructor(
    readonly org: OrgSim,
    readonly name: string,
    readonly id: Uint8Array,
    readonly rotateOnJoin: boolean,
  ) {}

  get idHex(): string {
    return toHex(this.id)
  }

  get epoch(): number {
    return this.tip.rec.epoch
  }

  async create(members: Party[]): Promise<Minted<Group>> {
    return await this.mint(members.map((p) => p.id), [], true)
  }

  /**
   * The next transition. rotate begins an epoch with a fresh key; it is
   * forced when anything is removed unless keepEpoch is set (to build what a
   * host must refuse). reuse begins an epoch under an earlier epoch's key.
   */
  async change(o: { add?: Party[]; remove?: Party[]; rotate?: boolean; keepEpoch?: boolean; reuse?: number; mutate?: (g: Group) => void } = {}): Promise<Minted<Group>> {
    const add = (o.add ?? []).map((p) => p.id)
    const remove = (o.remove ?? []).map((p) => p.id)
    const must = remove.length > 0 || (this.tip.rec.rotateOnJoin && add.length > 0)
    const rotate = o.keepEpoch === true ? false : (o.rotate ?? false) || must || o.reuse !== undefined
    return await this.mint(add, remove, rotate, o.reuse, o.mutate)
  }

  private async mint(add: Uint8Array[], remove: Uint8Array[], rotate: boolean, reuse?: number, mutate?: (g: Group) => void): Promise<Minted<Group>> {
    const first = this.epochs.size === 0
    const prev = first ? undefined : this.tip
    const seq = prev === undefined ? 0 : prev.rec.seq + 1
    const members = sortKeys([...this.members.filter((k) => !hasKey(remove, k)), ...add])
    const epoch = prev === undefined ? 0 : rotate ? prev.rec.epoch + 1 : prev.rec.epoch
    let key: Uint8Array
    if (prev === undefined || rotate) key = reuse !== undefined ? this.epochs.get(reuse)!.key : epochKey()
    else key = this.epochs.get(prev.rec.epoch)!.key
    const rec: Group = {
      hall: this.org.hall,
      org: this.org.party.id,
      group: this.id,
      seq,
      prev: prev === undefined ? new Uint8Array(32) : commitment(prev.tx),
      name: this.name,
      epoch,
      epochSeq: prev === undefined || rotate ? seq : prev.rec.epochSeq,
      commitment: commitmentOf(key),
      add: sortKeys(add),
      remove: sortKeys(remove),
      count: members.length,
      members: membersRoot(members),
      rotateOnJoin: this.rotateOnJoin,
      rotateEvery: 0,
      created: t0 + 60 + seq * 600,
      extra: [],
    }
    mutate?.(rec)
    const record = encodeGroup(rec)
    const input = prev === undefined ? { src: this.org.chainTree, vout: this.org.nextChainVout(), keyID: KeyChain } : { src: prev.tx, vout: 0, keyID: KeyGroup }
    const tx = await token(this.org.party, TagGroup, record, KeyGroup, [input])
    this.org.chain.mine(tx)
    const m: Minted<Group> = { tx, beef: tokenBEEF(tx, input.src), rec, record }
    if (prev === undefined || rotate) this.epochs.set(epoch, { key, epochId: epochID(record) })
    this.tip = m
    this.history.push(m)
    for (const k of remove) if (!hasKey(this.removed, k)) this.removed.push(k)
    this.members = members
    return m
  }

  /** Wrap carriers of the current epoch's key to recipients (every member by default), at most per entries each. */
  async wraps(recipients: Uint8Array[] = this.members, per = 16): Promise<Transaction[]> {
    const { key, epochId } = this.epochs.get(this.epoch)!
    const sorted = sortKeys(recipients)
    const out: Transaction[] = []
    for (let i = 0; i < sorted.length; i += per) {
      const rec = encodeWrap({
        hall: this.org.hall,
        org: this.org.party.id,
        group: this.id,
        epoch: this.epoch,
        epochId,
        entries: sorted.slice(i, i + per).map((m) => wrapEntry(this.org.party, m, epochId, this.epoch, key)),
        extra: [],
      })
      out.push(await carrier(this.org.recordTree, this.org.nextRecordVout(), this.org.party, KeyRecord, rec))
    }
    return out
  }
}

/**
 * A client wallet that can pay, as BRC-105's client asks of one: each
 * createAction spends a fresh coin mined on chain to the outputs asked for,
 * with change back to itself.
 */
export class PayingWallet extends ProtoWallet {
  constructor(
    private readonly key: PrivateKey,
    private readonly chain: Chain,
  ) {
    super(key)
  }

  coin(): Transaction {
    return this.chain.mine(fromNowhere([{ satoshis: 100000, lockingScript: new P2PKH().lock(this.key.toAddress()) }]))
  }

  async pay(outputs: Array<{ satoshis: number; lockingScript: string }>, coin = this.coin()): Promise<Transaction> {
    const tx = new Transaction(
      1,
      [{ sourceTransaction: coin, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(this.key), sequence: 0xffffffff }],
      [
        ...outputs.map((o) => ({ satoshis: o.satoshis, lockingScript: LockingScript.fromHex(o.lockingScript) })),
        { satoshis: 100000 - outputs.reduce((n, o) => n + o.satoshis, 0) - 100, lockingScript: new P2PKH().lock(this.key.toAddress()) },
      ],
      0,
    )
    await tx.sign()
    return tx
  }

  async createAction(args: CreateActionArgs): Promise<CreateActionResult> {
    const tx = await this.pay((args.outputs ?? []).map((o) => ({ satoshis: o.satoshis, lockingScript: o.lockingScript })))
    return { tx: tx.toAtomicBEEF(), txid: tx.id('hex') }
  }
}
