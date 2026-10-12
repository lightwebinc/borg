/**
 * The module inside the real overlay engine (the upstream release the
 * reference host's engine is a one-patch fork of, at the same version),
 * over an in-memory store: what the engine records for a refusal, the
 * suppression attacks spec section 11.1 guards against, a token refused
 * while the host's headers lag and admitted once they catch up, every golden
 * transaction, answers hydrated by the engine, restore from the engine's
 * own storage in any order, and a sibling host filled by replay from what
 * the first one stores.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Engine } from '@bsv/overlay'
import { Transaction } from '@bsv/sdk'
import { storedToken } from './admit.js'
import { tokenBEEF } from './beef.js'
import { KeyGrant, KeyRecord, LookupService, decodeOrg, encodeWrap } from './borgrec.js'
import { MemoryJournal } from './journal.js'
import type { BorgLookupService } from './ls_borg.js'
import { decodeContext, verifyGrant } from './merkle.js'
import { borgModule } from './module.js'
import { answer, config, hall, setup, submit, topic } from './testhost.js'
import { Chain, OrgSim, Party, cHex, carrier, sweep, wrapEntry } from './testmint.js'
import { FakeHost, MemoryStorage, beefOf, byName, txVectors } from './testutil.js'
import { fromHex, toHex } from './util.js'

const v = txVectors()

const applied = (s: MemoryStorage, txid: string): Promise<boolean> => s.doesAppliedTransactionExist({ txid, topic })

async function contexts(e: Engine, query: Record<string, unknown>): Promise<Array<number[] | undefined>> {
  const a = await e.lookup({ service: LookupService, query })
  if (a.type !== 'output-list') throw new Error('not an output list')
  return a.outputs.map((o) => (o as { context?: number[] }).context)
}

const orgHex = toHex(decodeOrg(fromHex(byName(v, 'org-token-genesis').admits!.record!)).org)

test('suppression: a manager that records refusals loses a genuine grant and token to a copy in a worse BEEF; tm_borg does not', async () => {
  for (const [worseName, genuineName] of [
    ['carrier-beef-tree-unproven', 'grant-carrier-beef-v1'],
    ['token-alone', 'org-token-revoke-predecessor-not-held'],
    ['token-parent-txid-only', 'org-token-revoke-predecessor-not-held'],
  ]) {
    const worse = byName(v, worseName!)
    const genuine = byName(v, genuineName!)
    assert.equal(worse.txid, genuine.txid)
    const naive = await setup({ naive: true })
    assert.deepEqual(await submit(naive.engine, beefOf(worse)), [])
    assert.ok(await applied(naive.storage, genuine.txid), `${worseName}: the refusal is recorded as applied`)
    const before = naive.calls()
    assert.deepEqual(await submit(naive.engine, beefOf(genuine)), [])
    assert.equal(naive.calls(), before, `${worseName}: the genuine object is a duplicate the manager never sees`)

    const real = await setup()
    assert.deepEqual(await submit(real.engine, beefOf(worse)), [], `${worseName}: raised, nothing admitted`)
    assert.equal(await applied(real.storage, genuine.txid), false, `${worseName}: nothing recorded`)
    assert.equal(real.host.count('borg_refused_total', { reason: 'beef' }), 1)
    assert.deepEqual(await submit(real.engine, beefOf(genuine)), [genuine.admits!.outputsToAdmit[0]!], `${worseName}: the genuine object is admitted`)
  }
})

test("a token whose parent's block the host's headers lack is refused without a trace, and admitted once they catch up", async () => {
  const chain = new Chain(71000)
  const org = new OrgSim(hall, 'lagging org', chain)
  const g0 = await org.orgToken([])
  const g1 = await org.orgToken([(await org.grant(new Party('lag member'))).c])
  // The host's headers: everything but the block of seq 0, the parent of seq 1.
  const height = g0.tx.merklePath!.blockHeight
  const root = chain.roots.get(height)!
  const s = await setup({ chain })
  chain.roots.delete(height)
  assert.deepEqual(await submit(s.engine, g1.beef), [], 'refused unmined')
  assert.equal(s.host.count('borg_refused_total', { reason: 'unmined' }), 1)
  assert.equal(await applied(s.storage, g1.tx.id('hex')), false)
  chain.roots.set(height, root)
  assert.deepEqual(await submit(s.engine, g1.beef), [0], 'decided again and admitted')
  assert.equal(s.ls.tokenStatus(g1.tx.id('hex')), 'pending', 'its predecessor is not held')
  assert.deepEqual(await submit(s.engine, g0.beef), [0])
  assert.equal(s.ls.tokenStatus(g1.tx.id('hex')), 'consistent')
})

test('every transaction vector through the engine: admitted as the vectors say, nothing refused recorded, and the index answers them', async () => {
  const { engine: e, storage, ls } = await setup()
  const admitted = new Set(v.cases.filter((t) => t.verdict === 'admit').map((t) => t.txid))
  const engineRefused: string[] = []
  for (const t of v.cases) {
    const got = await submit(e, beefOf(t))
    if (got === undefined) {
      // The engine's own SPV refuses before any manager runs: a proof at a
      // height its headers do not hold, a script its interpreter refuses.
      assert.equal(t.verdict, 'refuse', t.name)
      engineRefused.push(t.name)
    }
    if (t.verdict === 'admit') {
      assert.ok(await applied(storage, t.txid), t.name)
    } else if (!admitted.has(t.txid)) {
      assert.deepEqual(got ?? [], [], t.name)
      assert.equal(await applied(storage, t.txid), false, t.name)
    }
  }
  assert.equal(v.cases.length, 100)
  const id = (n: string): string => byName(v, n).txid
  const c = (n: string): string => byName(v, n).admits!.commitment!
  // Statuses: the sweep spends bob's grant outpoint; a second carrier on alice's is superseded.
  assert.equal(ls.status(c('grant-carrier-bob')), 'retracted')
  assert.equal(ls.status(c('grant-carrier-alice')), 'held')
  assert.equal(ls.status(c('carrier-locktime-max')), 'superseded')
  assert.equal(ls.status(c('wrap-carrier-epoch-1')), 'held')
  for (const n of ['org-token-genesis', 'org-token-revoke', 'group-token-create', 'group-token-add', 'group-token-remove-same-block']) {
    assert.equal(ls.tokenStatus(id(n)), 'consistent', n)
  }
  // Answers the engine hydrates.
  const group = '406504d3fbe1ae098f09a49dffa12222'
  const alice = '029c1f585aa80762f9f4458f6806978b7befb46c7b68af82d399630793ff1a2b50'
  const bob = '031428f3a3532ff4f1cac70f7292bfad06d1037f800ee8839b56ebba917a22e900'
  assert.deepEqual(await answer(e, { hall, org: orgHex }), [id('org-token-revoke')])
  assert.deepEqual(await answer(e, { hall, org: orgHex, seq: '0' }), [id('org-token-genesis')])
  assert.deepEqual(await answer(e, { hall, roll: orgHex }), [id('grant-carrier-alice'), id('grant-carrier-carol')])
  assert.deepEqual(await answer(e, { hall, org: orgHex, member: alice }), [id('grant-carrier-alice')])
  const [ctx] = await contexts(e, { hall, org: orgHex, member: alice })
  const ic = decodeContext(Uint8Array.from(ctx!))
  assert.equal(ic.seq, 1)
  assert.ok(verifyGrant(ic, fromHex(c('grant-carrier-alice')), decodeOrg(fromHex(byName(v, 'org-token-revoke').admits!.record!)).roll), 'the inclusion path verifies to the roll the answer used')
  assert.deepEqual(await answer(e, { hall, org: orgHex, member: bob }), [], 'bob was revoked')
  assert.deepEqual(await answer(e, { hall, groupsOf: orgHex }), [id('group-token-remove-same-block')])
  assert.deepEqual(await answer(e, { hall, org: orgHex, group }), [id('group-token-remove-same-block')])
  assert.deepEqual(await answer(e, { hall, org: orgHex, memberOf: alice }), [id('group-token-remove-same-block')])
  assert.deepEqual(await answer(e, { hall, org: orgHex, memberOf: bob }), [], 'bob was removed')
  assert.deepEqual(await answer(e, { hall, org: orgHex, group, epoch: '0' }), [id('wrap-carrier-epoch-0')])
  assert.deepEqual(await answer(e, { hall, org: orgHex, group, epoch: '1', member: alice }), [id('wrap-carrier-epoch-1')])
  assert.deepEqual(await answer(e, { hall, org: orgHex, group, epoch: '1', member: bob }), [], 'no wrap of epoch 1 names bob')
  assert.deepEqual(await answer(e, { hall, spent: v.cases.find((t) => t.name === 'sweep')!.admits!.spent![0]! }), [id('sweep')])
  assert.deepEqual(await answer(e, { hall, history: orgHex, chain: group }), [id('group-token-create'), id('group-token-add'), id('group-token-remove-same-block')])
  assert.ok(engineRefused.length > 0)
})

/** The questions a scenario asks, with the statuses it holds. */
async function snapshot(ls: BorgLookupService, questions: Array<Record<string, unknown>>, carriers: string[], tokens: string[]) {
  const answers = []
  for (const q of questions) answers.push(await ls.lookup({ service: LookupService, query: q }))
  return { answers, carriers: carriers.map((c) => ls.status(c)), tokens: tokens.map((t) => ls.tokenStatus(t)) }
}

test('restore from the engine storage reaches the same answers and statuses whatever order the rows come in, with or without their BEEF or the token rows', async () => {
  const chain = new Chain(72000, v.headers)
  const s = await setup({ chain })
  const org = new OrgSim(hall, 'restore org', chain)
  const [a, b, c] = [new Party('restore a'), new Party('restore b'), new Party('restore c')]
  const ga = await org.grant(a)
  const gb = await org.grant(b)
  const gc = await org.grant(c)
  const o0 = await org.orgToken([ga.c, gb.c, gc.c])
  const o1 = await org.orgToken([], [gb.c])
  const grp = await org.group('restore group', [a, b])
  const w0 = await grp.wraps()
  await grp.change({ add: [c] })
  const w0c = await grp.wraps([c.id])
  const g2 = await grp.change({ remove: [b] })
  const w1 = await grp.wraps()
  const sw = await sweep(org.grantTree, [gb.vout], org.party, KeyGrant, chain)
  // In order, so seq 1 spends a held seq 0 and the engine keeps seq 0 as a spent output.
  for (const beef of [o0.beef, o1.beef]) assert.deepEqual(await submit(s.engine, beef), [0])
  assert.equal(s.ls.tokenStatus(o1.tx.id('hex')), 'consistent')
  for (const x of [ga.tx, gb.tx, gc.tx, ...w0, ...w0c, ...w1]) assert.deepEqual(await submit(s.engine, x.toAtomicBEEF()), [0])
  // The group's tokens, the latest first: each pending until its parent arrives.
  const history = [...grp.history].reverse()
  assert.deepEqual(await submit(s.engine, history[0]!.beef), [0])
  assert.equal(s.ls.tokenStatus(g2.tx.id('hex')), 'pending')
  for (const m of history.slice(1)) assert.deepEqual(await submit(s.engine, m.beef), [0])
  assert.equal(s.ls.tokenStatus(g2.tx.id('hex')), 'consistent')
  assert.deepEqual(await submit(s.engine, sw.toAtomicBEEF()), [0])
  assert.deepEqual(await submit(s.engine, beefOf(byName(v, 'funding-tree-alone'))), [0], 'an unmatched spend, in the bounded table')

  const questions: Array<Record<string, unknown>> = [
    { hall, org: org.hex },
    { hall, roll: org.hex },
    { hall, org: org.hex, member: a.hex },
    { hall, groupsOf: org.hex },
    { hall, org: org.hex, memberOf: c.hex },
    { hall, org: org.hex, memberOf: b.hex },
    { hall, org: org.hex, group: grp.idHex, epoch: '0' },
    { hall, org: org.hex, group: grp.idHex, epoch: '1' },
    { hall, spent: `${org.grantTree.id('hex')}.${gb.vout}` },
    { hall, history: org.hex, chain: grp.idHex },
    { hall, spent: byName(v, 'funding-tree-alone').admits!.spent![0]! },
  ]
  const carriers = [ga, gb, gc].map((g) => toHex(g.c)).concat([...w0, ...w0c, ...w1].map(cHex))
  const tokens = [o0.tx, o1.tx, g2.tx].map((t) => t.id('hex'))
  const want = await snapshot(s.ls, questions, carriers, tokens)
  assert.deepEqual(want.tokens, ['consistent', 'consistent', 'consistent'])
  assert.deepEqual(want.answers[5], [], 'b was removed')
  assert.equal(want.answers[1]!.length, 2)

  const rows = await s.storage.findUTXOsForTopic(topic, undefined, undefined, true)
  assert.ok(!rows.some((o) => o.txid === o0.tx.id('hex')), 'a token its successor spent is not among the unspent rows')
  const bare = rows.map((o) => ({ ...o, beef: undefined }))
  const noTokens = new MemoryJournal()
  for (const l of s.journal.lines) if (l.k !== 't') noTokens.append(l)
  for (const [label, order, journal] of [
    ['as stored', rows, s.journal],
    ['reversed', [...rows].reverse(), s.journal],
    ['without BEEF', bare, s.journal],
    ['nothing handed over', [], s.journal],
    ['without the token rows: walked back through the stored tokens', rows, noTokens],
  ] as const) {
    const host = new FakeHost()
    const again = borgModule(host, config, [topic], journal, chain.tracker).ls
    assert.ok((await again.restore(order as never, s.storage)) > 0, label)
    const got = await snapshot(again, questions, carriers, tokens)
    if (order.length === 0) {
      // The bounded table is rebuilt from the tombstones the host hands
      // over and is never journaled, so a restore handed nothing has no
      // unmatched spends; everything the journal names is read back.
      assert.deepEqual(got.answers.pop(), [], label)
      assert.deepEqual(got, { ...want, answers: want.answers.slice(0, -1) }, label)
      continue
    }
    assert.deepEqual(got, want, label)
  }
})

test('a sibling host is filled by replay: each token assembled from the stored tokens of the first host, its parent from the seq before', async () => {
  const chain = new Chain(73000, v.headers)
  const first = await setup({ chain })
  const org = new OrgSim(hall, 'replay org', chain)
  const grp = await org.group('replay group', [new Party('replay a'), new Party('replay b')])
  await grp.change({ add: [new Party('replay c')] })
  await grp.change({ remove: [new Party('replay b')] })
  for (const m of grp.history) assert.deepEqual(await submit(first.engine, m.beef), [0])
  // What the first host answers is the token alone with its proof.
  const stored: Transaction[] = []
  for (let seq = 0; seq < 3; seq++) {
    const a = await first.engine.lookup({ service: LookupService, query: { hall, org: org.hex, group: grp.idHex, seq: String(seq) } })
    assert.equal(a.type, 'output-list')
    const o = (a as { outputs: Array<{ beef: number[] }> }).outputs[0]!
    const { tx } = storedToken(Uint8Array.from(o.beef))
    assert.equal(Transaction.fromBEEF(o.beef).id('hex'), tx.id('hex'))
    stored.push(tx)
  }
  const second = await setup({ chain })
  // Any order: the latest first, each pending until its parent arrives.
  assert.deepEqual(await submit(second.engine, tokenBEEF(stored[2]!, stored[1]!)), [0])
  assert.deepEqual(await submit(second.engine, tokenBEEF(stored[1]!, stored[0]!)), [0])
  assert.equal(second.ls.tokenStatus(stored[2]!.id('hex')), 'pending')
  // At seq 0 the parent is the chain tree, which no host keeps: from the publisher.
  assert.deepEqual(await submit(second.engine, tokenBEEF(stored[0]!, org.chainTree)), [0])
  assert.equal(second.ls.tokenStatus(stored[2]!.id('hex')), 'consistent')
  for (const q of [{ hall, org: org.hex, group: grp.idHex }, { hall, history: org.hex, chain: grp.idHex }]) {
    assert.deepEqual(await answer(second.engine, q), await answer(first.engine, q))
  }
})

test('engine answers are hydrated from storage, and a dropped carrier is deleted from storage and never restored', async () => {
  const chain = new Chain(74000, v.headers)
  const s = await setup({ chain })
  const org = new OrgSim(hall, 'evidence org', chain)
  const grp = await org.group('evidence group', [new Party('ev a')])
  const vout = org.nextRecordVout()
  // Ten wrap carriers on one funding output: the organization equivocated.
  const ep = grp.epochs.get(0)!
  const many: Transaction[] = []
  for (let i = 0; i < 10; i++) {
    const rec = encodeWrap({ hall, org: org.party.id, group: grp.id, epoch: 0, epochId: ep.epochId, entries: [wrapEntry(org.party, grp.members[0]!, ep.epochId, 0, ep.key)], extra: [] })
    many.push(await carrier(org.recordTree, vout, org.party, KeyRecord, rec))
  }
  for (const x of many) assert.deepEqual(await submit(s.engine, x.toAtomicBEEF()), [0])
  const sorted = [...many].sort((a, b) => (cHex(a) < cHex(b) ? -1 : 1))
  assert.deepEqual(await answer(s.engine, { hall, org: org.hex, group: grp.idHex, epoch: '0' }), [sorted[0]!.id('hex')], 'one answered carrier per funding output')
  // The winner and eight superseded are kept; the tenth was dropped and deleted.
  await s.ls.flush()
  assert.equal(await s.storage.findOutput(sorted[9]!.id('hex'), 0, topic), null)
  for (const x of sorted.slice(0, 9)) assert.notEqual(await s.storage.findOutput(x.id('hex'), 0, topic), null)
  assert.equal(s.host.count('borg_dropped_total', { why: 'evidence' }), 1)
  assert.deepEqual(await submit(s.engine, sorted[9]!.toAtomicBEEF()), [], 'offered again, a dropped carrier is a duplicate')
  const again = borgModule(new FakeHost(), config, [topic], s.journal, chain.tracker).ls
  await again.restore(await s.storage.findUTXOsForTopic(topic, undefined, undefined, true), s.storage)
  assert.equal(again.wrapCount, 9)
  // A carrier past the evidence bound leaves nothing behind: its output was
  // deleted, so a restore does not bring it back, and no journal line names
  // it, so the fresh module has no record of it at all. The refusal does not
  // need one: the row it meets is the same row, so it is refused again
  // (asserted above, before the restore).
  assert.equal(again.status(cHex(sorted[9]!)), undefined, 'not kept, and so not answered')
  assert.equal(again.lineCount, 9, 'a line for each carrier the row keeps')
  // This carrier was kept when it arrived and pushed out later by a lower
  // one, so its `c` line is in the journal: the journal is append-only and
  // is not compacted, so a restart replays the line, rebuilds the row and
  // applies the bound again. Memory is bounded either way; the journal is
  // spared only for a carrier refused on arrival.
  assert.equal(again.forgottenLines, 1, 'the bound is applied again on the rebuilt row')
})
