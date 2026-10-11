/**
 * tm_borg on its own: every golden transaction admitted or refused through
 * the topic manager exactly as the vectors say, every refusal record of
 * refusal-v1.json refused inside a real carrier or token for its reason, a
 * BEEF over the host's bound, a sweep over the host's input bound, a header
 * source that cannot answer, and every refusal label a host counts shown to
 * be reachable.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { P2PKH, PrivateKey, Transaction } from '@bsv/sdk'
import { KeyChain, KeyGrant, KeyGroup, KeyOrg, KeyPolicy, KeyRecord, TagGroup, TagOrg, encodeGrant } from './borgrec.js'
import { DefaultMaxBEEF, tokenBEEF } from './beef.js'
import { HeaderTracker } from '@lightwebinc/bcommon/host'
import { Chain, OrgSim, Party, carrier, fromNowhere, fundingTree, sweep, token } from './testmint.js'
import { FakeHost, beefOf, byName, heldOf, refusalVectors, tracker, txVectors } from './testutil.js'
import { BorgTopicManager, Reasons, Refused } from './tm_borg.js'
import { fromHex } from './util.js'

const v = txVectors()
const seen = new Set<string>()

async function verdict(tm: BorgTopicManager, beef: number[] | Uint8Array, held: number[]): Promise<{ admit?: { outputsToAdmit: number[]; coinsToRetain: number[] }; reason?: string }> {
  try {
    return { admit: await tm.identifyAdmissibleOutputs(Array.from(beef), held) }
  } catch (e) {
    if (!(e instanceof Refused)) throw e
    seen.add(e.reason)
    return { reason: e.reason }
  }
}

test('every transaction vector is admitted or refused, raising, as the vectors say', async () => {
  const host = new FakeHost()
  const headers = tracker(v.headers)
  let admitted = 0
  let refused = 0
  const managers = new Map<number, BorgTopicManager>()
  for (const t of v.cases) {
    const bound = t.maxSweepInputs ?? v.maxSweepInputs
    if (!managers.has(bound)) managers.set(bound, new BorgTopicManager(v.topic, host, headers, v.maxBeef, bound))
    const tm = managers.get(bound)!
    const got = await verdict(tm, beefOf(t), heldOf(t))
    if (t.verdict === 'admit') {
      assert.deepEqual(got.admit, { outputsToAdmit: t.admits!.outputsToAdmit, coinsToRetain: t.admits!.coinsToRetain }, t.name)
      admitted++
    } else {
      assert.equal(got.reason, t.reason, t.name)
      refused++
    }
  }
  assert.equal(admitted + refused, 95)
  assert.equal(v.cases.length, 95)
  // A txid admitted before in another BEEF is counted once, and once as a
  // repeat. The sweep at its own bound is decided by a second manager,
  // which counts it apart.
  for (const [kind, distinct, repeats] of [['grant', 4, 1], ['wrap', 2, 0], ['org-token', 2, 1], ['group-token', 3, 1], ['sweep', 3, 0]] as const) {
    assert.equal(host.count('borg_admitted_total', { kind }), distinct, kind)
    assert.equal(host.count('borg_admitted_repeats_total', { kind }), repeats, kind)
  }
  assert.equal(host.count('borg_refused_total', { reason: 'beef' }), 16)
})

/** The vectors' organisation: 32 bytes of 0x51, a public test key. */
const vectorOrg = new Party('vector org', PrivateKey.fromHex('51'.repeat(32)))

test('every refusal record, carried by an otherwise valid carrier or token, is refused for its reason', async () => {
  const chain = new Chain(60000, v.headers)
  const tm = new BorgTopicManager(v.topic, new FakeHost(), chain.tracker)
  const vectors = refusalVectors()
  const grants = fundingTree(vectorOrg, KeyGrant, vectors.length, chain)
  const records = fundingTree(vectorOrg, KeyRecord, vectors.length, chain)
  const policies = fundingTree(vectorOrg, KeyPolicy, vectors.length, chain)
  const chains = fundingTree(vectorOrg, KeyChain, vectors.length, chain)
  for (const [i, r] of vectors.entries()) {
    const rec = fromHex(r.hex)
    let beef: Uint8Array | number[]
    if (r.record === 'grant') {
      beef = (await carrier(grants, i, vectorOrg, KeyGrant, rec)).toAtomicBEEF()
    } else if (r.record === 'wrap') {
      beef = (await carrier(records, i, vectorOrg, KeyRecord, rec)).toAtomicBEEF()
    } else if (r.record === 'policy') {
      beef = (await carrier(policies, i, vectorOrg, KeyPolicy, rec)).toAtomicBEEF()
    } else {
      const tag = r.record === 'org' ? TagOrg : TagGroup
      const tk = await token(vectorOrg, tag, rec, r.record === 'org' ? KeyOrg : KeyGroup, [{ src: chains, vout: i, keyID: KeyChain }])
      chain.mine(tk)
      beef = tokenBEEF(tk, chains)
    }
    // A content-stage grant names a stand-in revocation outpoint no
    // transaction spends, so a carrier refuses it at the first content rule;
    // the transaction vectors carry grant-signature and grant-reveal refusals
    // on the outpoints their grants name.
    const want = r.stage === 'content' ? 'grant-revocation' : r.reason
    assert.equal((await verdict(tm, beef, [])).reason, want, `${r.record}: ${r.label}`)
  }
})

test('a BEEF over the host bound is refused beef, whatever it carries; bytes that are no BEEF too', async () => {
  const chain = new Chain(61000)
  const org = new OrgSim('example_hall_vkqzmtrwxp', 'big tree owner', chain, { trees: 1 })
  // 6000 funding outputs of 49 bytes and more: the carrier's BEEF is over 256 KiB.
  const tree = fundingTree(org.party, KeyGrant, 6000, chain)
  const g = await org.grant(new Party('subject'), undefined, { vout: 0 })
  const beef = (await carrier(tree, 0, org.party, KeyGrant, encodeGrant(g.grant))).toAtomicBEEF()
  assert.ok(beef.length > DefaultMaxBEEF)
  const tm = new BorgTopicManager(v.topic, new FakeHost(), chain.tracker)
  assert.equal((await verdict(tm, beef, [])).reason, 'beef')
  assert.equal((await verdict(tm, [1, 2, 3], [])).reason, 'beef')
})

test('a sweep over the host input bound is refused too-large; a plain payment is not-borg', async () => {
  const chain = new Chain(62000)
  const owner = new Party('sweeper')
  const tree = fundingTree(owner, KeyRecord, 1025, chain)
  const big = await sweep(tree, Array.from({ length: 1025 }, (_, i) => i), owner, KeyRecord, chain)
  const tm = new BorgTopicManager(v.topic, new FakeHost(), chain.tracker)
  assert.equal((await verdict(tm, big.toAtomicBEEF(), [])).reason, 'too-large')
  const payer = new Party('payer')
  const coin = chain.mine(fromNowhere([{ satoshis: 5000, lockingScript: new P2PKH().lock(payer.key.toAddress()) }]))
  const plain = new Transaction(1, [{ sourceTransaction: coin, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(payer.key), sequence: 0xffffffff }], [{ satoshis: 4000, lockingScript: new P2PKH().lock(owner.key.toAddress()) }], 0)
  await plain.sign()
  assert.equal((await verdict(tm, plain.toAtomicBEEF(), [])).reason, 'not-borg')
})

test('a header source that cannot answer refuses a token unmined, raising, so it is decided again later', async () => {
  const host = new FakeHost()
  const tm = new BorgTopicManager(v.topic, host, new HeaderTracker('http://127.0.0.1:9', 500))
  const t = byName(v, 'org-token-genesis')
  assert.equal((await verdict(tm, beefOf(t), heldOf(t))).reason, 'unmined')
  assert.equal(host.count('borg_refused_total', { reason: 'unmined' }), 1)
  const ok = new BorgTopicManager(v.topic, host, tracker(v.headers))
  assert.deepEqual((await verdict(ok, beefOf(t), heldOf(t))).admit, { outputsToAdmit: [0], coinsToRetain: [] })
})

test('every refusal label a host counts is reachable, and each has a test above', () => {
  assert.deepEqual(Reasons.filter((r) => !seen.has(r)), [])
})

test('the manager needs no inputs, documents itself, and refuses a topic that is not a hall', async () => {
  const tm = new BorgTopicManager(v.topic, new FakeHost(), tracker(v.headers))
  assert.equal(tm.hall, v.hall)
  assert.deepEqual(await tm.identifyNeededInputs(), [])
  assert.match(await tm.getDocumentation(), /raised/)
  assert.deepEqual(await tm.getMetaData(), { name: v.topic, shortDescription: `Admits the borg hall ${v.hall}.` })
  assert.throws(() => new BorgTopicManager('tm_other_x_abcdefghij', new FakeHost(), tracker(v.headers)))
  assert.throws(() => new BorgTopicManager('tm_borg_Bad_abcdefghij', new FakeHost(), tracker(v.headers)))
  assert.throws(() => new BorgTopicManager(v.topic, new FakeHost(), tracker(v.headers), 0))
})

test('an admission is counted once per transaction; a repeat the engine lets through is counted apart', async () => {
  const host = new FakeHost()
  const tm = new BorgTopicManager(v.topic, host, tracker(v.headers))
  const s = byName(v, 'sweep')
  for (let i = 0; i < 3; i++) await verdict(tm, beefOf(s), heldOf(s))
  assert.equal(host.count('borg_admitted_total', { kind: 'sweep' }), 1)
  assert.equal(host.count('borg_admitted_repeats_total', { kind: 'sweep' }), 2)
})
