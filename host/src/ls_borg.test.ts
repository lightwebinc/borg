/**
 * ls_borg through the real engine: every lookup class answered, paged and
 * refused as spec section 10.2 says; each token status of section 11.2 and
 * the answers that follow from it (pending, a fork, a broken fold); the
 * bounded table of spends no carrier used; the evidence kept per outpoint;
 * and retention at its floors (section 11.3).
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction } from '@bsv/sdk'
import { KeyGrant, KeyPolicy, KeyRecord, LookupService, encodeGrant } from './borgrec.js'
import { decodeContext, policyRoot, rollRoot, verifyGrant, verifyPolicy } from './merkle.js'
import { MemoryJournal } from './journal.js'
import { borgModule } from './module.js'
import { answer, config, hall, setup, submit, topic } from './testhost.js'
import { Chain, OrgSim, Party, cHex, carrier, commitment, fundingTree, issueGrant, sweep } from './testmint.js'
import { FakeHost } from './testutil.js'
import { toHex } from './util.js'

const id = (t: Transaction): string => t.id('hex')
const byTxid = (txs: Transaction[]): string[] => txs.map(id).sort()

test('every free class answers, pages and moves its cursor as spec section 10.2 says', async () => {
  const chain = new Chain(80000)
  const s = await setup({ chain })
  const org = new OrgSim(hall, 'lookup org', chain, { trees: 80 })
  const people = Array.from({ length: 69 }, (_, i) => new Party(`lookup member ${i}`))
  const twice = people[0]!
  const grants = [] as Awaited<ReturnType<OrgSim['grant']>>[]
  for (const p of people) grants.push(await org.grant(p))
  grants.push(await org.grant(twice, { expiry: '2099-12-31T23:59:59Z', role: 'admin', scope: 'member,admin' }))
  for (const g of grants) assert.deepEqual(await submit(s.engine, g.tx.toAtomicBEEF()), [0])
  const o0 = await org.orgToken(grants.slice(0, 64).map((g) => g.c))
  const o1 = await org.orgToken(grants.slice(64).map((g) => g.c))
  for (const m of [o0, o1]) assert.deepEqual(await submit(s.engine, m.beef), [0])

  // org, org-seq.
  assert.deepEqual(await answer(s.engine, { hall, org: org.hex }), [id(o1.tx)])
  assert.deepEqual(await answer(s.engine, { hall, org: org.hex, seq: '0' }), [id(o0.tx)])
  assert.deepEqual(await answer(s.engine, { hall, org: org.hex, seq: '7' }), [])
  // roll, roll-after: 70 grants, 64 a page, by txid.
  const all = byTxid(grants.map((g) => g.tx))
  const page1 = await answer(s.engine, { hall, roll: org.hex })
  assert.deepEqual(page1, all.slice(0, 64))
  assert.deepEqual(await answer(s.engine, { hall, roll: org.hex, after: page1[63]! }), all.slice(64))
  assert.deepEqual(await answer(s.engine, { hall, roll: org.hex, after: all[69]! }), [])
  // member: both of a subject's grants, each with an inclusion path to the roll.
  const a = await s.engine.lookup({ service: LookupService, query: { hall, org: org.hex, member: twice.hex } })
  assert.equal(a.type, 'output-list')
  const outs = (a as { outputs: Array<{ beef: number[]; context?: number[] }> }).outputs
  assert.equal(outs.length, 2)
  for (const o of outs) {
    const ctx = decodeContext(Uint8Array.from(o.context!))
    assert.deepEqual([ctx.seq, ctx.size], [1, 70])
    assert.ok(verifyGrant(ctx, commitment(Transaction.fromBEEF(o.beef)), o1.rec.roll))
  }

  // Groups: three, one of them without people[1].
  const g1 = await org.group('first', [people[1]!, people[2]!])
  const g2 = await org.group('second', [people[2]!])
  const g3 = await org.group('third', [people[1]!, people[3]!])
  await g3.change({ add: [people[4]!] })
  for (const g of [g1, g2, g3]) for (const m of g.history) assert.deepEqual(await submit(s.engine, m.beef), [0])
  const ids = [g1, g2, g3].sort((x, y) => (x.idHex < y.idHex ? -1 : 1))
  assert.deepEqual(await answer(s.engine, { hall, groupsOf: org.hex }), ids.map((g) => id(g.tip.tx)))
  assert.deepEqual(await answer(s.engine, { hall, groupsOf: org.hex, after: ids[0]!.idHex }), ids.slice(1).map((g) => id(g.tip.tx)))
  const with1 = ids.filter((g) => g !== g2)
  assert.deepEqual(await answer(s.engine, { hall, org: org.hex, memberOf: people[1]!.hex }), with1.map((g) => id(g.tip.tx)))
  assert.deepEqual(await answer(s.engine, { hall, org: org.hex, memberOf: people[1]!.hex, after: with1[0]!.idHex }), with1.slice(1).map((g) => id(g.tip.tx)))
  assert.deepEqual(await answer(s.engine, { hall, org: org.hex, memberOf: people[50]!.hex }), [])
  assert.deepEqual(await answer(s.engine, { hall, org: org.hex, group: g3.idHex }), [id(g3.tip.tx)])
  assert.deepEqual(await answer(s.engine, { hall, org: org.hex, group: g3.idHex, seq: '0' }), [id(g3.history[0]!.tx)])

  // wraps, wraps-after, wrap: 66 recipients one to a record, 64 a page.
  const big = await org.group('big', people.slice(0, 60))
  await big.change({ add: people.slice(60, 66) })
  for (const m of big.history) await submit(s.engine, m.beef)
  const wraps = await big.wraps(undefined, 1)
  for (const w of wraps) assert.deepEqual(await submit(s.engine, w.toAtomicBEEF()), [0])
  const wAll = byTxid(wraps)
  const wq = { hall, org: org.hex, group: big.idHex, epoch: '0' }
  assert.deepEqual(await answer(s.engine, wq), wAll.slice(0, 64))
  assert.deepEqual(await answer(s.engine, { ...wq, after: wAll[63]! }), wAll.slice(64))
  const to5 = wraps.find((w) => Transaction.fromBEEF(w.toAtomicBEEF()).outputs[0]!.lockingScript.toHex().includes(people[5]!.hex))!
  assert.deepEqual(await answer(s.engine, { ...wq, member: people[5]!.hex }), [id(to5)])
  assert.deepEqual(await answer(s.engine, { ...wq, epoch: '1' }), [])

  // sweep: a mined sweep of two grant-tree outputs answers for each.
  const sw = await sweep(org.grantTree, [grants[5]!.vout, 79], org.party, KeyGrant, chain)
  assert.deepEqual(await submit(s.engine, sw.toAtomicBEEF()), [0])
  assert.deepEqual(await answer(s.engine, { hall, spent: `${id(org.grantTree)}.${grants[5]!.vout}` }), [id(sw)])
  assert.deepEqual(await answer(s.engine, { hall, spent: `${id(org.grantTree)}.79` }), [id(sw)])
  // The roll answers the swept grant still: it is in the roll whatever its status (spec 11.2).
  assert.equal(s.ls.status(toHex(grants[5]!.c)), 'retracted')
  assert.ok((await answer(s.engine, { hall, roll: org.hex })).concat(await answer(s.engine, { hall, roll: org.hex, after: page1[63]! })).includes(id(grants[5]!.tx)))
})

test('history pages a chain by seq, never splitting one, and is refused through the engine where it is priced', async () => {
  const chain = new Chain(81000)
  const s = await setup({ chain })
  const org = new OrgSim(hall, 'history org', chain)
  const g = await org.group('long', [new Party('h a')])
  for (let i = 0; i < 65; i++) await g.change({ rotate: true })
  for (const m of g.history) assert.deepEqual(await submit(s.engine, m.beef), [0])
  const all = g.history.map((m) => id(m.tx))
  assert.equal(all.length, 66)
  const q = { hall, history: org.hex, chain: g.idHex }
  assert.deepEqual(await answer(s.engine, q), all.slice(0, 64))
  assert.deepEqual(await answer(s.engine, { ...q, after: '63' }), all.slice(64))
  assert.deepEqual(await answer(s.engine, { ...q, after: '65' }), [])
  const o = await org.orgToken([])
  await submit(s.engine, o.beef)
  assert.deepEqual(await answer(s.engine, { hall, history: org.hex, chain: 'org' }), [id(o.tx)])
  // Priced: the engine's own route refuses it.
  const priced = borgModule(new FakeHost(), { ...config, prices: new Map([['history', 5], ['history-after', 5]]) }, [topic], new MemoryJournal(), chain.tracker).ls
  await priced.restore([], s.storage)
  await assert.rejects(priced.lookup({ service: LookupService, query: q }), /priced/)
})

test('a question is refused unless it is exactly one class with valid values on a hall the host carries', async () => {
  const s = await setup()
  const org = '02'.padEnd(66, '1')
  const key = '029c1f585aa80762f9f4458f6806978b7befb46c7b68af82d399630793ff1a2b50'
  for (const [query, why] of [
    [{ hall: 'other_hall_abcdefghij', org: key }, /does not carry/],
    [{ hall, org: key, extra: 'x' }, /no class/],
    [{ hall, org: key, history: key, chain: 'org' }, /no class/],
    [{ hall, history: key, chain: 'org', org: key }, /no class/],
    [{ hall, org: key, seq: '01' }, /seq/],
    [{ hall, org: key, seq: 7 }, /not a string/],
    [{ hall, org: org }, /org/],
    [{ hall, spent: 'nope' }, /spent/],
    [{ hall, roll: key, after: 'g'.repeat(64) }, /after/],
    ['not an object', /query/],
  ] as const) {
    await assert.rejects(s.ls.lookup({ service: LookupService, query }), why as RegExp, JSON.stringify(query))
  }
  await assert.rejects(s.ls.lookup({ service: 'ls_other', query: { hall, org: key } }), /ls_borg/)
})

test('statuses: pending answers from the highest complete fold, a fork from the first mined, and a broken fold stays broken', async () => {
  const chain = new Chain(82000)
  const s = await setup({ chain })
  const org = new OrgSim(hall, 'status org', chain)
  const [a, b, c] = [new Party('st a'), new Party('st b'), new Party('st c')]
  const ga = await org.grant(a)
  const gb = await org.grant(b)
  const gc = await org.grant(c)
  for (const g of [ga, gb, gc]) await submit(s.engine, g.tx.toAtomicBEEF())
  const o0 = await org.orgToken([ga.c])
  const o1 = await org.orgToken([gb.c])
  const o2 = await org.orgToken([gc.c])
  const o3 = await org.orgToken([], [ga.c])
  for (const m of [o0, o1, o3]) assert.deepEqual(await submit(s.engine, m.beef), [0])
  // seq 3 arrived before seq 2: pending; the latest answer is seq 3, the roll is seq 1's.
  assert.equal(s.ls.tokenStatus(id(o3.tx)), 'pending')
  assert.equal(s.host.gauges.get('borg_tokens_pending')?.(), 1)
  assert.deepEqual(await answer(s.engine, { hall, org: org.hex }), [id(o3.tx)])
  assert.deepEqual(await answer(s.engine, { hall, roll: org.hex }), byTxid([ga.tx, gb.tx]))
  assert.deepEqual(await submit(s.engine, o2.beef), [0])
  assert.equal(s.ls.tokenStatus(id(o3.tx)), 'consistent')
  assert.deepEqual(await answer(s.engine, { hall, roll: org.hex }), byTxid([gb.tx, gc.tx]))

  // A fork at seq 1 of a group (two tokens on one predecessor, as a
  // reorganisation leaves): both answered as latest, the roll of the first mined used.
  const g = await org.group('fork', [a])
  const g0 = g.tip
  const left = await g.change({ add: [b] })
  // Build the other branch on the same predecessor.
  g.tip = g0
  g.members = [a.id]
  const right = await g.change({ add: [c] })
  for (const m of [g0, right, left]) assert.deepEqual(await submit(s.engine, m.beef), [0])
  assert.deepEqual((await answer(s.engine, { hall, org: org.hex, group: g.idHex })).sort(), [id(left.tx), id(right.tx)].sort())
  const first = left.tx.merklePath!.blockHeight < right.tx.merklePath!.blockHeight ? b : c
  const other = first === b ? c : b
  assert.deepEqual(await answer(s.engine, { hall, org: org.hex, memberOf: first.hex }), [id((first === b ? left : right).tx)])
  assert.deepEqual(await answer(s.engine, { hall, org: org.hex, memberOf: other.hex }), [])

  // roll-mismatch: a token whose roll root is wrong is admitted (the host reads only its predecessor) and flagged; so is its successor.
  const bad = await org.orgToken([], [], { mutate: (r) => (r.roll = rollRoot([])) })
  const after = await org.orgToken([])
  for (const m of [bad, after]) assert.deepEqual(await submit(s.engine, m.beef), [0])
  assert.equal(s.ls.tokenStatus(id(bad.tx)), 'roll-mismatch')
  assert.equal(s.ls.tokenStatus(id(after.tx)), 'roll-mismatch')
  assert.ok(s.host.count('borg_token_status_total', { status: 'roll-mismatch' }) >= 2)
})

test('statuses: commitment-reuse, rejoin-unrotated and members-mismatch are reported, never refused', async () => {
  const chain = new Chain(83000)
  const s = await setup({ chain })
  const org = new OrgSim(hall, 'group status org', chain)
  const [a, b, c] = [new Party('gs a'), new Party('gs b'), new Party('gs c')]
  const g = await org.group('statuses', [a, b])
  await g.change({ remove: [b] })
  const rejoin = await g.change({ add: [b], keepEpoch: true })
  const reuse = await g.change({ reuse: 0 })
  const broken = await g.change({ add: [c], mutate: (r) => (r.members = rollRoot([])) })
  const later = await g.change({ rotate: true })
  for (const m of g.history) assert.deepEqual(await submit(s.engine, m.beef), [0])
  assert.equal(s.ls.tokenStatus(id(rejoin.tx)), 'rejoin-unrotated')
  assert.equal(s.ls.tokenStatus(id(reuse.tx)), 'commitment-reuse')
  assert.equal(s.ls.tokenStatus(id(broken.tx)), 'members-mismatch')
  assert.equal(s.ls.tokenStatus(id(later.tx)), 'members-mismatch', 'a broken fold stays broken')
  assert.equal(s.ls.tokenStatus(id(g.history[1]!.tx)), 'consistent')
  for (const status of ['rejoin-unrotated', 'commitment-reuse', 'members-mismatch']) assert.ok(s.host.count('borg_token_status_total', { status }) >= 1, status)
  assert.ok(s.host.lines.some((l) => l.msg === 'ls_borg token status' && l.extra?.['status'] === 'rejoin-unrotated'))
})

test('the bounded table: a sweep before its carrier still retracts it; a stranger\'s chain of sweeps costs the table and no more', async () => {
  const chain = new Chain(84000)
  const s = await setup({ chain, spendTable: 3 })
  const org = new OrgSim(hall, 'table org', chain)
  // The sweep arrives first: its spend waits in the table, then joins the carrier's row for life.
  const ga = await org.grant(new Party('table a'))
  const early = await sweep(org.grantTree, [ga.vout], org.party, KeyGrant, chain)
  assert.deepEqual(await submit(s.engine, early.toAtomicBEEF()), [0])
  assert.equal(s.ls.tableSize, 1)
  assert.deepEqual(await submit(s.engine, ga.tx.toAtomicBEEF()), [0])
  assert.equal(s.ls.status(toHex(ga.c)), 'retracted')
  assert.equal(s.ls.tableSize, 0)
  assert.ok(s.journal.lines.some((l) => l.k === 's' && l.s === id(early)))
  // A stranger mines five sweeps of its own outputs: the table keeps the newest three.
  const stranger = new Party('stranger')
  const tree = fundingTree(stranger, KeyRecord, 5, chain)
  const sweeps: Transaction[] = []
  for (let i = 0; i < 5; i++) sweeps.push(await sweep(tree, [i], stranger, KeyRecord, chain))
  for (const x of sweeps) assert.deepEqual(await submit(s.engine, x.toAtomicBEEF()), [0])
  assert.equal(s.ls.tableSize, 3)
  assert.equal(s.host.count('borg_dropped_total', { why: 'table' }), 2)
  await s.ls.flush()
  for (const [i, x] of sweeps.entries()) {
    const kept = i >= 2
    assert.equal((await s.storage.findOutput(id(x), 0, topic)) !== null, kept, `sweep ${i}`)
    assert.deepEqual(await answer(s.engine, { hall, spent: `${id(tree)}.${i}` }), kept ? [id(x)] : [], `sweep ${i}`)
  }
  assert.ok(!s.journal.lines.some((l) => l.k === 's' && sweeps.some((x) => id(x) === l.s)), 'no stranger spend is journaled')
  // The retraction outlives a restart; the table is rebuilt from the kept tombstones.
  const again = borgModule(new FakeHost(), { ...config, spendTable: 3 }, [topic], s.journal, chain.tracker).ls
  await again.restore(await s.storage.findUTXOsForTopic(topic, undefined, undefined, true), s.storage)
  assert.equal(again.status(toHex(ga.c)), 'retracted')
  assert.equal(again.tableSize, 3)
})

test('evidence: 64 superseded grants are kept on an outpoint, the 65th dropped', async () => {
  const chain = new Chain(85000)
  const s = await setup({ chain })
  const org = new OrgSim(hall, 'evidence org', chain, { trees: 2 })
  const subject = new Party('evidence subject')
  const g = await issueGrant(hall, org.party, subject, org.grantTree, 0)
  const rec = encodeGrant(g)
  const many: Transaction[] = []
  for (let i = 0; i < 66; i++) many.push(await carrier(org.grantTree, 0, org.party, KeyGrant, rec, 4102444800 + i))
  for (const x of many) assert.deepEqual(await submit(s.engine, x.toAtomicBEEF()), [0])
  const sorted = [...many].sort((a, b) => (cHex(a) < cHex(b) ? -1 : 1))
  assert.equal(s.ls.status(cHex(sorted[0]!)), 'held')
  assert.equal(s.ls.grantCount, 65)
  assert.ok(s.ls.dropped(cHex(sorted[65]!)))
  assert.equal(s.host.count('borg_dropped_total', { why: 'evidence' }), 1)
})

test('retention: a revoked or retracted grant and an ended epoch\'s wraps go after the floor; the roll, the current epoch and an unadmitted grant stay', async () => {
  let now = 1_900_000_000
  const day = 86400
  const chain = new Chain(86000)
  const s = await setup({ chain, now: () => now })
  const org = new OrgSim(hall, 'retention org', chain)
  const [a, b, c, d] = [new Party('ret a'), new Party('ret b'), new Party('ret c'), new Party('ret d')]
  const [ga, gb, gc, gd] = [await org.grant(a), await org.grant(b), await org.grant(c), await org.grant(d)]
  for (const x of [ga, gb, gc, gd]) await submit(s.engine, x.tx.toAtomicBEEF())
  const o0 = await org.orgToken([ga.c, gb.c, gc.c])
  await submit(s.engine, o0.beef)
  const grp = await org.group('kept', [a, b])
  await submit(s.engine, grp.tip.beef)
  const w0 = await grp.wraps()
  for (const w of w0) await submit(s.engine, w.toAtomicBEEF())
  now += 10 * day
  const o1 = await org.orgToken([], [gb.c])
  const sw = await sweep(org.grantTree, [gc.vout], org.party, KeyGrant, chain)
  const g1 = await grp.change({ remove: [b] })
  const w1 = await grp.wraps()
  for (const beef of [o1.beef, sw.toAtomicBEEF(), g1.beef, ...w1.map((w) => w.toAtomicBEEF())]) assert.deepEqual(await submit(s.engine, beef), [0])
  now += 30 * day
  assert.equal(await s.ls.prune(), 0, 'under the floor nothing goes')
  now += 1 * day
  assert.equal(await s.ls.prune(), 1 + w0.length, "b's grant and epoch 0's wraps")
  assert.ok(s.ls.dropped(toHex(gb.c)), 'revoked 31 days ago')
  assert.equal(s.ls.dropped(toHex(gc.c)), false, 'swept, but the roll still holds it')
  assert.equal(s.ls.dropped(toHex(ga.c)), false, 'in the roll')
  assert.equal(s.ls.dropped(toHex(gd.c)), false, 'never admitted, never swept')
  for (const w of w0) assert.ok(s.ls.dropped(cHex(w)), 'epoch 0 ended 31 days ago')
  for (const w of w1) assert.equal(s.ls.dropped(cHex(w)), false, 'the current epoch')
  assert.equal(await s.storage.findOutput(id(gb.tx), 0, topic), null, 'deleted from storage')
  // Tokens are kept for the host's life.
  assert.equal(s.ls.tokenCount, 4)
  assert.deepEqual(await answer(s.engine, { hall, org: org.hex, seq: '0' }), [id(o0.tx)])
})

test('policy, role and bound answer the set the latest token commits to, each with a path into the policy root', async () => {
  const chain = new Chain(80000)
  const s = await setup({ chain })
  const org = new OrgSim(hall, 'policy org', chain)
  const alice = new Party('policy alice')
  const gid = new Uint8Array(16).fill(7)

  const def = await org.policyRecord('releaser', { permissions: ['bsecret/release/payroll', 'bstore/get/*'] })
  const other = await org.policyRecord('moderator', { permissions: ['bchat/moderate/general'] })
  const toGroup = await org.policyRecord('releaser', { to: gid })
  const toKey = await org.policyRecord('releaser', { to: alice.id, permissions: ['bstore/get/*'] })
  const retired = await org.policyRecord('moderator', { to: alice.id })
  for (const p of [def, other, toGroup, toKey, retired]) {
    assert.deepEqual(await submit(s.engine, p.tx.toAtomicBEEF()), [0])
  }

  // seq 0 defines all five, seq 1 retires one: the answers follow the set
  // the latest token commits to, never what the host happens to hold.
  const o0 = await org.orgToken([], [], { define: [def.c, other.c, toGroup.c, toKey.c, retired.c] })
  assert.deepEqual(await submit(s.engine, o0.beef), [0])
  assert.deepEqual(byTxid([def.tx, other.tx, toGroup.tx, toKey.tx, retired.tx]), (await answer(s.engine, { hall, policy: org.hex })).slice())
  const o1 = await org.orgToken([], [], { retire: [retired.c] })
  assert.deepEqual(await submit(s.engine, o1.beef), [0])
  assert.deepEqual(await answer(s.engine, { hall, policy: org.hex }), byTxid([def.tx, other.tx, toGroup.tx, toKey.tx]))

  // role: the definition and every binding that names it.
  assert.deepEqual(await answer(s.engine, { hall, org: org.hex, role: 'releaser' }), byTxid([def.tx, toGroup.tx, toKey.tx]))
  // A retired record leaves every answer, whatever still names it.
  assert.deepEqual(await answer(s.engine, { hall, org: org.hex, role: 'moderator' }), byTxid([other.tx]))
  // bound: by group identifier and by key, told apart by length.
  assert.deepEqual(await answer(s.engine, { hall, org: org.hex, boundTo: toHex(gid) }), byTxid([toGroup.tx]))
  assert.deepEqual(await answer(s.engine, { hall, org: org.hex, boundTo: alice.hex }), byTxid([toKey.tx]))
  // A cursor moves past what it names.
  const page = await answer(s.engine, { hall, org: org.hex, role: 'releaser' })
  assert.deepEqual(await answer(s.engine, { hall, org: org.hex, role: 'releaser', after: page[0]! }), page.slice(1))

  // Every role and bound answer carries a path from the record's commitment
  // to the policy root of the token its seq names, and to no other root.
  const a = await s.engine.lookup({ service: LookupService, query: { hall, org: org.hex, role: 'releaser' } })
  const outs = (a as { outputs: Array<{ beef: number[]; context?: number[] }> }).outputs
  assert.equal(outs.length, 3)
  for (const o of outs) {
    const ctx = decodeContext(Uint8Array.from(o.context!))
    assert.deepEqual([ctx.seq, ctx.size], [1, 4])
    const c = commitment(Transaction.fromBEEF(o.beef))
    assert.ok(verifyPolicy(ctx, c, o1.rec.policy), 'verifies against the policy root of its seq')
    assert.ok(!verifyPolicy(ctx, c, o0.rec.policy), 'and against no earlier root')
    assert.ok(!verifyGrant(ctx, c, o1.rec.roll), 'and never against the roll')
  }
})

test('a sweep retracts a policy carrier and the next token drops it from the root: a reader sees both halves', async () => {
  const chain = new Chain(80000)
  const s = await setup({ chain })
  const org = new OrgSim(hall, 'policy sweep org', chain)
  const def = await org.policyRecord('releaser', { permissions: ['s/a/b'] })
  const keep = await org.policyRecord('auditor', { permissions: ['s/read/b'] })
  for (const p of [def, keep]) assert.deepEqual(await submit(s.engine, p.tx.toAtomicBEEF()), [0])
  const o0 = await org.orgToken([], [], { define: [def.c, keep.c] })
  assert.deepEqual(await submit(s.engine, o0.beef), [0])
  assert.deepEqual([s.ls.policyCount, s.ls.wrapCount], [2, 0])
  assert.deepEqual(await answer(s.engine, { hall, policy: org.hex }), byTxid([def.tx, keep.tx]))

  // The sweep is the fast half: the carrier is retracted at once, and a
  // reader learns it from the sweep class, not from the answer going quiet.
  // The policy classes answer every record the root holds, whatever its
  // status, so that a reader computing the root sees each and checks it.
  const sw = await sweep(org.policyTree, [0], org.party, KeyPolicy, chain)
  assert.deepEqual(await submit(s.engine, sw.toAtomicBEEF()), [0])
  assert.deepEqual(await answer(s.engine, { hall, policy: org.hex }), byTxid([def.tx, keep.tx]))
  assert.deepEqual(await answer(s.engine, { hall, spent: `${org.policyTree.id('hex')}.0` }), [sw.id('hex')])

  // The committed half: the organisation's next token drops the commitment,
  // and only then does the record leave every answer.
  const o1 = await org.orgToken([], [], { retire: [def.c] })
  assert.deepEqual(await submit(s.engine, o1.beef), [0])
  assert.deepEqual(await answer(s.engine, { hall, policy: org.hex }), byTxid([keep.tx]))
  assert.deepEqual(await answer(s.engine, { hall, org: org.hex, role: 'releaser' }), [])
})

test('retention: a policy record stays while the set holds it, and goes the floor after it is retired or retracted', async () => {
  const chain = new Chain(80000)
  let clock = 1_700_000_000
  const s = await setup({ chain, now: () => clock })
  const org = new OrgSim(hall, 'policy retention org', chain)
  const kept = await org.policyRecord('auditor', { permissions: ['s/read/a'] })
  const retired = await org.policyRecord('releaser', { permissions: ['s/a/b'] })
  const swept = await org.policyRecord('moderator', { permissions: ['s/mod/a'], vout: 5 })
  for (const p of [kept, retired, swept]) assert.deepEqual(await submit(s.engine, p.tx.toAtomicBEEF()), [0])
  assert.deepEqual(await submit(s.engine, (await org.orgToken([], [], { define: [kept.c, retired.c, swept.c] })).beef), [0])
  assert.equal(s.ls.policyCount, 3)

  // Nothing goes while the set holds all three, however long the host waits.
  clock += 400 * 86400
  assert.equal(await s.ls.prune(), 0)
  assert.equal(s.ls.policyCount, 3)

  // One is retired by a token, one is retracted by a sweep while the set
  // still names it. The retired one goes once the floor has passed; the
  // retracted one is kept, because a reader computing the policy root still
  // needs every record the root holds, as a swept grant in the roll is kept.
  assert.deepEqual(await submit(s.engine, (await org.orgToken([], [], { retire: [retired.c] })).beef), [0])
  const sw = await sweep(org.policyTree, [5], org.party, KeyPolicy, chain)
  assert.deepEqual(await submit(s.engine, sw.toAtomicBEEF()), [0])
  assert.equal(await s.ls.prune(), 0)
  assert.equal(s.ls.policyCount, 3)
  clock += 32 * 86400
  assert.equal(await s.ls.prune(), 1)
  assert.deepEqual([s.ls.policyCount, (await answer(s.engine, { hall, policy: org.hex })).length], [2, 2])

  // Once the root drops the retracted one too, the floor runs again and it
  // goes; what the set still holds never does.
  assert.deepEqual(await submit(s.engine, (await org.orgToken([], [], { retire: [swept.c] })).beef), [0])
  clock += 32 * 86400
  assert.equal(await s.ls.prune(), 1)
  assert.equal(s.ls.policyCount, 1)
  assert.deepEqual(await answer(s.engine, { hall, policy: org.hex }), byTxid([kept.tx]))
})

test('a token is counted once however often the chain is folded again', async () => {
  const chain = new Chain(80000)
  const s = await setup({ chain })
  const org = new OrgSim(hall, 'counting org', chain, { trees: 40 })
  const people = Array.from({ length: 3 }, (_, i) => new Party(`counting member ${i}`))
  const grants = []
  for (const p of people) grants.push(await org.grant(p))
  for (const g of grants) assert.deepEqual(await submit(s.engine, g.tx.toAtomicBEEF()), [0])
  assert.deepEqual(await submit(s.engine, (await org.orgToken(grants.map((g) => g.c))).beef), [0])
  const o1 = await org.orgToken([], [grants[0]!.c])
  assert.deepEqual(await submit(s.engine, o1.beef), [0])
  const counted = (): number => s.host.count('borg_token_status_total', { status: 'consistent' })
  assert.equal(counted(), 2, 'two tokens, counted once each')

  // Nine tokens at one seq: the bound keeps eight and forgets one, which
  // clears every status in the chain and decides them all again. A token
  // already counted is not counted a second time for the same status, so
  // the total is the tokens the host holds and no more.
  for (let i = 0; i < 9; i++) {
    const fork = await org.orgToken([], [], { mutate: (r) => (r.created += i + 1) })
    org.tip = o1
    await submit(s.engine, fork.beef)
  }
  // Eleven: the two, and each of the nine forks counted when it was
  // decided, the one the bound later turned away included. Before the fix
  // the refold after that bound counted every token of the chain again.
  assert.equal(counted(), 11, `counted ${counted()} consistent tokens`)
})

test('policy-mismatch: a token whose policy root does not reproduce is reported, the roll is decided first, and both are sticky', async () => {
  const chain = new Chain(83000)
  const s = await setup({ chain })
  const org = new OrgSim(hall, 'policy status org', chain)
  const def = await org.policyRecord('reader', { permissions: ['s/get/a'] })
  const other = await org.policyRecord('writer', { permissions: ['s/put/a'] })
  for (const p of [def, other]) assert.deepEqual(await submit(s.engine, p.tx.toAtomicBEEF()), [0])

  // seq 0 defines one record and says so; seq 1 defines the second but
  // commits to a root that is not the set's.
  const o0 = await org.orgToken([], [], { define: [def.c] })
  assert.deepEqual(await submit(s.engine, o0.beef), [0])
  const bad = await org.orgToken([], [], { define: [other.c], mutate: (r) => (r.policy = policyRoot([])) })
  assert.deepEqual(await submit(s.engine, bad.beef), [0])
  const after = await org.orgToken([], [])
  assert.deepEqual(await submit(s.engine, after.beef), [0])

  assert.equal(s.ls.tokenStatus(id(o0.tx)), 'consistent')
  assert.equal(s.ls.tokenStatus(id(bad.tx)), 'policy-mismatch')
  assert.equal(s.ls.tokenStatus(id(after.tx)), 'policy-mismatch', 'a broken policy fold stays broken')
  assert.ok(s.host.count('borg_token_status_total', { status: 'policy-mismatch' }) >= 1)
  assert.ok(s.host.lines.some((l) => l.msg === 'ls_borg token status' && l.extra?.['status'] === 'policy-mismatch'))
  // The host answers from the fold and reports the status; it does not
  // decide. A fold that broke on the root still holds what it applied
  // before the check, as a broken roll does, so both records are answered
  // and a reader that reads the status refuses the chain from there on
  // (spec section 13).
  assert.deepEqual(await answer(s.engine, { hall, policy: org.hex }), byTxid([def.tx, other.tx]))
})

test('the roll is decided before the policy set: a token that breaks both is roll-mismatch', async () => {
  const chain = new Chain(83000)
  const s = await setup({ chain })
  const org = new OrgSim(hall, 'both broken org', chain)
  const def = await org.policyRecord('reader', { permissions: ['s/get/a'] })
  assert.deepEqual(await submit(s.engine, def.tx.toAtomicBEEF()), [0])
  const both = await org.orgToken([], [], {
    define: [def.c],
    mutate: (r) => {
      r.roll = policyRoot([new Uint8Array(32).fill(9)])
      r.policy = policyRoot([])
    },
  })
  assert.deepEqual(await submit(s.engine, both.beef), [0])
  assert.equal(s.ls.tokenStatus(id(both.tx)), 'roll-mismatch')
})
