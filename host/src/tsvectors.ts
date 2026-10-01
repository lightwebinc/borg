/**
 * The vectors the TypeScript codec writes, for the Go codec to read: the
 * other direction of the cross-language check. Every input is fixed, so a
 * run writes the same bytes every time; tsvectors.test.ts regenerates the
 * file and compares it byte for byte, and the Go tests decode, check,
 * open and admit each value.
 *
 *   node dist/tsvectors.js          # write testdata/ts/ts-v1.json
 *   node dist/tsvectors.js -check   # compare, write nothing
 */
import { createCipheriv } from 'node:crypto'
import { readFileSync, writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import {
  Certificate,
  CompletedProtoWallet,
  Hash,
  KeyDeriver,
  LockingScript,
  MerklePath,
  P2PKH,
  PrivateKey,
  PushDrop,
  Transaction,
  UnlockingScript,
} from '@bsv/sdk'
import { CborMap, LockTime, compareBytes, readerLockingKey } from '@lightwebinc/bcommon'
import {
  KeyChain,
  KeyGrant,
  KeyGroup,
  KeyOrg,
  KeyRecord,
  MagicGrant,
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
  newHall,
  releaseKeyID,
  type Grant,
} from './borgrec.js'
import { tokenBEEF } from './beef.js'
import { commitmentOf, epochID, releaseKeyIDString } from './epoch.js'
import { membersRoot, rollRoot } from './merkle.js'
import { fundingScript, pushDropScript } from './script.js'
import { concat, displayTxid, fromHex, toBase64, toHex } from './util.js'

export const file = new URL('../../testdata/ts/ts-v1.json', import.meta.url)

const text = new TextEncoder()
const sha = (s: string): Uint8Array => Uint8Array.from(Hash.sha256(Array.from(text.encode(s))))

/** The test keys of the Go vectors: 0x51 the organisation, 0x52 alice, 0x53 bob. */
const orgKey = PrivateKey.fromHex('51'.repeat(32))
const aliceKey = PrivateKey.fromHex('52'.repeat(32))
const bobKey = PrivateKey.fromHex('53'.repeat(32))
const pub = (k: PrivateKey): Uint8Array => Uint8Array.from(k.toPublicKey().encode(true) as number[])
const org = pub(orgKey)
const orgWallet = new CompletedProtoWallet(orgKey)

const Hall = 'example_hall_vkqzmtrwxp'
const t0 = 1798761600

/** AES-256-GCM with a 32-byte IV: BRC-2's symmetric form under a fixed IV. */
function seal(key: Uint8Array, iv: Uint8Array, plaintext: Uint8Array): Uint8Array {
  const c = createCipheriv('aes-256-gcm', key, iv)
  return concat([iv, c.update(plaintext), c.final(), c.getAuthTag()])
}

/** A grant issued entirely by the TypeScript SDK, its random values fixed. */
async function issueGrant(subject: PrivateKey, values: Record<string, string>, revTxid: Uint8Array, revVout: number, reveal: boolean): Promise<Grant> {
  const name = toHex(pub(subject))
  const fields = new Map<string, Uint8Array>()
  const keyring = new Map<string, Uint8Array>()
  const rev = new Map<string, Uint8Array>()
  const deriver = new KeyDeriver(orgKey)
  for (const f of ['expiry', 'role', 'scope']) {
    const fk = sha(`borg/ts-vector/fieldkey/${name}/${f}`)
    fields.set(f, seal(fk, sha(`borg/ts-vector/iv/field/${name}/${f}`), text.encode(values[f]!)))
    const sk = deriver.deriveSymmetricKey([2, 'certificate field encryption'], f, toHex(pub(subject)))
    keyring.set(f, seal(Uint8Array.from(sk.toArray('be', 32)), sha(`borg/ts-vector/iv/keyring/${name}/${f}`), fk))
    rev.set(f, fk)
  }
  const g: Grant = {
    hall: Hall,
    org,
    subject: pub(subject),
    serial: sha(`borg/ts-vector/serial/${name}`),
    revTxid,
    revVout,
    fields,
    signature: new Uint8Array(0),
    keyring,
    reveal: reveal ? rev : undefined,
    extra: [],
  }
  const fieldsB64: Record<string, string> = {}
  for (const [f, c] of fields) fieldsB64[f] = toBase64(c)
  const cert = new Certificate(grantTypeBase64(), toBase64(g.serial), toHex(g.subject), toHex(org), `${displayTxid(revTxid)}.${revVout}`, fieldsB64)
  await cert.sign(orgWallet)
  g.signature = fromHex(cert.signature as string)
  return g
}

/** A release of k to member under a fixed IV. */
function wrapEntry(member: Uint8Array, epochId: Uint8Array, epoch: number, k: Uint8Array, iv: Uint8Array): Uint8Array {
  const sk = new KeyDeriver(orgKey).deriveSymmetricKey(ReleaseProtocol, releaseKeyIDString(epochId, epoch), toHex(member))
  return concat([WrapVersion, org, member, releaseKeyID(epochId, epoch), seal(Uint8Array.from(sk.toArray('be', 32)), iv, k)])
}

const key = (id: string): Uint8Array => Uint8Array.from(readerLockingKey(Protocol, id, toHex(org)).encode(true) as number[])

async function fieldSig(id: string, data: Uint8Array): Promise<Uint8Array> {
  const r = await orgWallet.createSignature({ data: Array.from(data), protocolID: Protocol, keyID: id, counterparty: 'anyone' })
  return Uint8Array.from(r.signature)
}

const ls = (b: Uint8Array): LockingScript => LockingScript.fromBinary(Array.from(b))

/** A transaction from nowhere: one unsigned input of a fixed outpoint. */
function fromNowhere(tag: string, outs: Array<{ satoshis: number; lockingScript: LockingScript }>): Transaction {
  return new Transaction(1, [{ sourceTXID: toHex(sha(`borg/ts-vector/nowhere/${tag}`)), sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff }], outs, 0)
}

const headers: Array<{ height: number; merkleRoot: string }> = []

/** A block of two at height: the transaction at offset 1 beside a fixed sibling. */
function mine(tx: Transaction, height: number): void {
  tx.merklePath = new MerklePath(height, [
    [
      { offset: 0, hash: '55'.repeat(32) },
      { offset: 1, hash: tx.id('hex'), txid: true },
    ],
  ])
  headers.push({ height, merkleRoot: tx.merklePath.computeRoot(tx.id('hex')) })
}

/** a and b in one block of four, at offsets 1 and 2, each with its own minimal path. */
function mineTogether(a: Transaction, b: Transaction, height: number): void {
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
  headers.push({ height, merkleRoot: a.merklePath.computeRoot(a.id('hex')) })
}

export async function generate(): Promise<string> {
  headers.length = 0
  // A hall drawn from fixed bytes: 234 and above are skipped.
  const random = Uint8Array.from([0xff, 0x00, 0xea, 0x19, 0xfe, 0x1a, 0xeb, 0x33, 0xf0, 0x4e, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06])
  let at = 0
  const drawn = newHall('ts_written', (n) => random.subarray(at, (at += n)))

  const pd = new PushDrop(orgWallet)
  const changeKey = PrivateKey.fromHex(toHex(sha('borg/ts-vector/change')))
  const change = new P2PKH().lock(changeKey.toAddress())

  // The trees: grants, wraps, chains; four outputs each, mined.
  const tree = (tag: string, id: string, height: number): Transaction => {
    const t = fromNowhere(tag, [0, 1, 2, 3].map(() => ({ satoshis: 1000, lockingScript: ls(fundingScript(key(id))) })))
    mine(t, height)
    return t
  }
  const grantTree = tree('grants', KeyGrant, 700)
  const recordTree = tree('records', KeyRecord, 701)
  const chainTree = tree('chains', KeyChain, 702)
  const coin = fromNowhere('coin', [{ satoshis: 20000, lockingScript: change }])
  mine(coin, 703)

  // Two grants, alice's revealed, bob's not, on grant-tree outputs 0 and 1.
  const alice = await issueGrant(aliceKey, { expiry: '2028-06-30T12:00:00Z', role: 'ingénieure', scope: 'member,admin' }, Uint8Array.from(grantTree.hash() as number[]), 0, true)
  const bob = await issueGrant(bobKey, { expiry: '2028-06-30T12:00:00Z', role: 'reader', scope: 'member' }, Uint8Array.from(grantTree.hash() as number[]), 1, false)
  const aliceRec = encodeGrant(alice)
  const bobRec = encodeGrant(bob)

  // A grant carrier signed through the SDK's PushDrop unlocker.
  const carrier = async (rec: Uint8Array, id: string, src: Transaction, vout: number): Promise<Transaction> => {
    const lock = pushDropScript(key(id), [rec], await fieldSig(id, rec))
    const tx = new Transaction(
      1,
      [{ sourceTransaction: src, sourceOutputIndex: vout, unlockingScriptTemplate: pd.unlock(Protocol, id, 'anyone', 'all', false), sequence: 0 }],
      [{ satoshis: 1000, lockingScript: ls(lock) }],
      LockTime,
    )
    await tx.sign()
    return tx
  }
  const aliceCarrier = await carrier(aliceRec, KeyGrant, grantTree, 0)
  const bobCarrier = await carrier(bobRec, KeyGrant, grantTree, 1)
  const cA = Uint8Array.from(aliceCarrier.hash() as number[])
  const cB = Uint8Array.from(bobCarrier.hash() as number[])

  // The organisation's seq 0 and seq 1, mined in one block: the BEEF of seq
  // 1 then holds one path merged by the SDK.
  const commits = [cA, cB].sort(compareBytes)
  const o0 = encodeOrg({
    hall: Hall,
    org,
    seq: 0,
    prev: new Uint8Array(32),
    name: 'Écrit en TypeScript',
    admit: commits,
    revoke: [],
    count: 2,
    roll: rollRoot(commits),
    opaque: false,
    created: t0,
    extra: [{ key: 40n, val: new CborMap([{ key: 'later', val: true }]) }],
  })
  const token = async (tag: Uint8Array, rec: Uint8Array, id: string, inputs: Array<{ src: Transaction; vout: number; id: string }>, extraSats: number): Promise<Transaction> => {
    const lock = pushDropScript(key(id), [tag, rec], await fieldSig(id, concat([tag, rec])))
    const tx = new Transaction(
      1,
      inputs.map((i) => ({
        sourceTransaction: i.src,
        sourceOutputIndex: i.vout,
        unlockingScriptTemplate: i.id === 'fee' ? new P2PKH().unlock(changeKey) : pd.unlock(Protocol, i.id, 'anyone', 'all', false),
        sequence: 0xffffffff,
      })),
      [
        { satoshis: 1, lockingScript: ls(lock) },
        { satoshis: extraSats, lockingScript: change },
      ],
      0,
    )
    await tx.sign()
    return tx
  }
  const org0 = await token(TagOrg, o0, KeyOrg, [{ src: chainTree, vout: 0, id: KeyChain }], 1000 - 1 - 300)
  const o1 = encodeOrg({
    hall: Hall,
    org,
    seq: 1,
    prev: Uint8Array.from(org0.hash() as number[]),
    name: 'Écrit en TypeScript',
    admit: [],
    revoke: [cB],
    count: 1,
    roll: rollRoot([cA]),
    opaque: false,
    created: t0 + 3600,
    extra: [],
  })
  const org1 = await token(TagOrg, o1, KeyOrg, [{ src: org0, vout: 0, id: KeyOrg }, { src: coin, vout: 0, id: 'fee' }], 20000 + 1 - 1 - 400)
  mineTogether(org0, org1, 704)

  // A group of alice and bob, epoch 0, and its wrap.
  const epochKey = sha('borg/ts-vector/epoch/0')
  const members = [pub(aliceKey), pub(bobKey)].sort(compareBytes)
  const g0 = encodeGroup({
    hall: Hall,
    org,
    group: sha('borg/ts-vector/group').subarray(0, 16),
    seq: 0,
    prev: new Uint8Array(32),
    name: 'Équipe',
    epoch: 0,
    epochSeq: 0,
    commitment: commitmentOf(epochKey),
    add: members,
    remove: [],
    count: 2,
    members: membersRoot(members),
    rotateOnJoin: true,
    rotateEvery: 604800,
    created: t0 + 60,
    extra: [],
  })
  const group0 = await token(TagGroup, g0, KeyGroup, [{ src: chainTree, vout: 1, id: KeyChain }], 1000 - 1 - 300)
  mine(group0, 705)
  const eid = epochID(g0)
  const wrapRec = encodeWrap({
    hall: Hall,
    org,
    group: sha('borg/ts-vector/group').subarray(0, 16),
    epoch: 0,
    epochId: eid,
    entries: members.map((m) => wrapEntry(m, eid, 0, epochKey, sha(`borg/ts-vector/iv/wrap/${toHex(m)}`))),
    extra: [{ key: 7n, val: 'kept' }],
  })
  const wrapCarrier = await carrier(wrapRec, KeyRecord, recordTree, 0)

  const hx = (b: Uint8Array): string => toHex(b)
  const beef = (tx: Transaction): string => hx(Uint8Array.from(tx.toAtomicBEEF()))
  const v = {
    description:
      'Written by the TypeScript codec (host/src/tsvectors.ts) and read by the Go codec: the hall draw reproduces from the same random bytes; each record decodes and re-encodes to the same bytes, unknown keys kept; ' +
      "the grants (issued, encrypted and signed by the TypeScript SDK) verify as certificates, open for their subjects and, where revealed, for anyone; the wrap opens for each member and matches the group token's commitment; " +
      'every transaction (carriers signed through the SDK PushDrop unlocker; organisation tokens 0 and 1 mined in one block, so the BEEF of token 1 holds one path the SDK merged) is admitted against the headers below; tokenBeef is the TypeScript tokenBEEF of each token and its parent.',
    hall: Hall,
    hallDraw: { name: 'ts_written', random: hx(random), hall: drawn },
    epochKey: hx(epochKey),
    records: { org0: hx(o0), org1: hx(o1), group0: hx(g0), grantAlice: hx(aliceRec), grantBob: hx(bobRec), wrap: hx(wrapRec) },
    grantValues: {
      alice: { expiry: '2028-06-30T12:00:00Z', role: 'ingénieure', scope: 'member,admin' },
      bob: { expiry: '2028-06-30T12:00:00Z', role: 'reader', scope: 'member' },
    },
    magicGrant: hx(MagicGrant),
    headers,
    transactions: [
      { name: 'grant-alice', txid: aliceCarrier.id('hex'), beef: beef(aliceCarrier), kind: 'grant' },
      { name: 'grant-bob', txid: bobCarrier.id('hex'), beef: beef(bobCarrier), kind: 'grant' },
      { name: 'wrap', txid: wrapCarrier.id('hex'), beef: beef(wrapCarrier), kind: 'wrap' },
      { name: 'org-0', txid: org0.id('hex'), beef: hx(tokenBEEF(org0, chainTree)), kind: 'org-token' },
      { name: 'org-1-same-block', txid: org1.id('hex'), beef: hx(tokenBEEF(org1, org0)), kind: 'org-token' },
      { name: 'group-0', txid: group0.id('hex'), beef: hx(tokenBEEF(group0, chainTree)), kind: 'group-token' },
    ],
  }
  return JSON.stringify(v, null, 2) + '\n'
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const out = await generate()
  if (process.argv.includes('-check')) {
    let have = ''
    try {
      have = readFileSync(file, 'utf8')
    } catch {}
    if (have !== out) {
      console.error(`differs: ${fileURLToPath(file)}`)
      process.exit(1)
    }
  } else {
    writeFileSync(file, out)
  }
}

