/**
 * Defects a review of a sibling host's index proved against it, each pinned
 * here with the sequence that shows it in this one: what a restart must not
 * lose, what arrival order must not decide, what one topic must not take
 * from another, and what must not wait in a queue.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import type { Transaction } from '@bsv/sdk'
import type { Output } from '@lightwebinc/bcommon'
import { tokenBEEF } from './beef.js'
import { KeyChain, KeyGrant, KeyOrg, KeyRecord, LookupService, TagOrg, encodeGrant, encodeOrg, encodeWrap, reasonOf } from './borgrec.js'
import { MemoryJournal } from './journal.js'
import { borgModule } from './module.js'
import { parseQuery } from './query.js'
import { answer, config, hall, setup, submit, topic } from './testhost.js'
import { Chain, OrgSim, Party, cHex, carrier, commitment, fundingTree, issueGrant, sweep, token, wrapEntry } from './testmint.js'
import { FakeHost, MemoryStorage } from './testutil.js'
import { fromHex, toHex } from './util.js'

const id = (t: Transaction): string => t.id('hex')

async function restored(s: Awaited<ReturnType<typeof setup>>, chain: Chain, journal = s.journal) {
  const ls = borgModule(new FakeHost(), config, [topic], journal, chain.tracker).ls
  await ls.restore(await s.storage.findUTXOsForTopic(topic, undefined, undefined, true), s.storage)
  return ls
}

/** n wrap carriers on one funding output of the organisation's record tree, lowest commitment first. */
async function equivocations(org: OrgSim, name: string, n: number): Promise<Transaction[]> {
  const grp = await org.group(name, [new Party(`${name} member`)])
  const vout = org.nextRecordVout()
  const ep = grp.epochs.get(0)!
  const many: Transaction[] = []
  for (let i = 0; i < n; i++) {
    const rec = encodeWrap({ hall: org.hall, org: org.party.id, group: grp.id, epoch: 0, epochId: ep.epochId, entries: [wrapEntry(org.party, grp.members[0]!, ep.epochId, 0, ep.key)], extra: [] })
    many.push(await carrier(org.recordTree, vout, org.party, KeyRecord, rec))
  }
  return many.sort((a, b) => (cHex(a) < cHex(b) ? -1 : 1))
}

test('a sweep that waits in the table still retracts after a restart, although a later transaction spent its tombstone', async () => {
  const chain = new Chain(93000)
  const s = await setup({ chain })
  const org = new OrgSim(hall, 'sweep org', chain)
  const ga = await org.grant(new Party('swept before it is published here'))
  const op = `${id(org.grantTree)}.${ga.vout}`
  // The sweep arrives before the carrier it retracts, and waits in the table.
  const s1 = await sweep(org.grantTree, [ga.vout], org.party, KeyGrant, chain)
  assert.deepEqual(await submit(s.engine, s1.toAtomicBEEF()), [0])
  assert.equal(s.ls.tableSize, 1)
  // The organisation funds its next grant from the sweep's output: storage now holds the tombstone as spent.
  const gb = await issueGrant(hall, org.party, new Party('funded by the tombstone'), s1, 0)
  const b = await carrier(s1, 0, org.party, KeyGrant, encodeGrant(gb))
  assert.deepEqual(await submit(s.engine, b.toAtomicBEEF()), [0])
  assert.equal((await s.storage.findUTXOsForTopic(topic)).some((o) => o.txid === id(s1)), false, 'not among the unspent rows a restore is handed')
  assert.deepEqual(s.journal.lines.filter((l) => l.k === 'w'), [{ k: 'w', txid: id(s1), hall }], 'the journal names it, once')
  const again = await restored(s, chain)
  assert.deepEqual([again.tableSize, again.sweepCount], [s.ls.tableSize, s.ls.sweepCount], 'the table is the same after a restart')
  assert.deepEqual((await again.lookup({ service: LookupService, query: { hall, spent: op } })).map((f) => f.txid), [id(s1)])
  // The retracted carrier arrives afterwards, at the live host and at the restored one.
  assert.deepEqual(await submit(s.engine, ga.tx.toAtomicBEEF()), [0])
  await again.outputAdmittedByTopic({ mode: 'whole-tx', topic, atomicBEEF: ga.tx.toAtomicBEEF(), outputIndex: 0 })
  assert.deepEqual([s.ls.status(toHex(ga.c)), again.status(toHex(ga.c))], ['retracted', 'retracted'])
  // A second restart replays the same journal: nothing is named twice.
  await restored(s, chain)
  assert.equal(s.journal.lines.filter((l) => l.k === 'w').length, 1)
})

test('a journal written before spent sweeps were named restores as it did', async () => {
  const chain = new Chain(93500)
  const s = await setup({ chain })
  const org = new OrgSim(hall, 'old journal org', chain)
  const ga = await org.grant(new Party('old a'))
  assert.deepEqual(await submit(s.engine, ga.tx.toAtomicBEEF()), [0])
  const o0 = await org.orgToken([ga.c])
  assert.deepEqual(await submit(s.engine, o0.beef), [0])
  const sw = await sweep(org.grantTree, [ga.vout], org.party, KeyGrant, chain)
  assert.deepEqual(await submit(s.engine, sw.toAtomicBEEF()), [0])
  assert.deepEqual([...new Set(s.journal.lines.map((l) => l.k))].sort(), ['c', 's', 't'], 'the line kinds every earlier journal holds')
  const again = await restored(s, chain)
  assert.deepEqual([again.status(toHex(ga.c)), again.tokenStatus(id(o0.tx)), again.sweepCount, again.rowCount], ['retracted', 'consistent', 1, 1])
})

test('a token turned away from a full seq while it was pending is read again once its predecessor is decided', async () => {
  const chain = new Chain(92000)
  const s = await setup({ chain })
  // The real token at seq 2 has to sort above eight others at its seq, and a txid is as good as drawn at random. So
  // the organisation's chain is minted again, on new trees, until that token's txid is in the upper half of them all:
  // a rogue then sorts below it at least every second time.
  const real = async () => {
    for (let tries = 0; tries < 64; tries++) {
      const org = new OrgSim(hall, 'bound org', chain)
      const tokens = [await org.orgToken([]), await org.orgToken([]), await org.orgToken([]), await org.orgToken([])] as const
      if (id(tokens[2].tx) >= '8') return { org, tokens }
    }
    throw new Error('64 chains were minted and none has a seq 2 token whose txid is in the upper half')
  }
  const {
    org,
    tokens: [o0, o1, o2, o3],
  } = await real()
  // The organisation's own tokens at seq 2, each on a parent no host admits and on a tree of its own, so there is
  // always another to mint, until eight have a txid below the real one's.
  const rogues: Array<{ beef: Uint8Array; txid: string }> = []
  for (let n = 0; rogues.filter((r) => r.txid < id(o2.tx)).length < 8; n++) {
    assert.ok(n < 512, `512 rogue tokens were minted and fewer than eight have a txid below the real one's ${id(o2.tx)}`)
    const spare = fundingTree(org.party, KeyChain, 1, chain)
    const x = await token(org.party, TagOrg, encodeOrg({ ...o1.rec, prev: fromHex((n + 1).toString(16).padStart(64, '0')) }), KeyOrg, [{ src: spare, vout: 0, keyID: KeyChain }])
    chain.mine(x)
    const y = await token(org.party, TagOrg, encodeOrg({ ...o2.rec, prev: commitment(x) }), KeyOrg, [{ src: x, vout: 0, keyID: KeyOrg }])
    chain.mine(y)
    rogues.push({ beef: tokenBEEF(y, x), txid: id(y) })
  }
  for (const r of rogues) assert.deepEqual(await submit(s.engine, r.beef), [0])
  // A replay in any order: the last tokens first.
  assert.deepEqual(await submit(s.engine, o3.beef), [0])
  assert.deepEqual(await submit(s.engine, o2.beef), [0])
  assert.equal(s.ls.tokenStatus(id(o2.tx)), undefined, 'pending, and above eight lower txids: the seq holds no more')
  assert.deepEqual(await submit(s.engine, o0.beef), [0])
  assert.deepEqual(await submit(s.engine, o1.beef), [0])
  // Its predecessor is decided now: the host reads it back from its own storage, and the chain is whole.
  assert.deepEqual([s.ls.tokenStatus(id(o2.tx)), s.ls.tokenStatus(id(o3.tx))], ['consistent', 'consistent'])
  const q = { hall, org: org.hex }
  assert.deepEqual(await answer(s.engine, q), [id(o3.tx)])
  const at2 = await answer(s.engine, { ...q, seq: '2' })
  assert.ok(at2.includes(id(o2.tx)))
  assert.equal(at2.length, 8)
  // The same as a host given the chain in order, and as a restart.
  const inOrder = await setup({ chain })
  for (const r of rogues) await submit(inOrder.engine, r.beef)
  for (const t of [o0, o1, o2, o3]) assert.deepEqual(await submit(inOrder.engine, t.beef), [0])
  assert.equal(inOrder.ls.tokenStatus(id(o3.tx)), 'consistent')
  assert.deepEqual(await answer(inOrder.engine, { ...q, seq: '2' }), at2)
  const again = await restored(s, chain)
  assert.deepEqual((await again.lookup({ service: LookupService, query: q })).map((f) => f.txid), [id(o3.tx)])
  assert.deepEqual((await again.lookup({ service: LookupService, query: { ...q, seq: '2' } })).map((f) => f.txid).sort(), [...at2].sort())
  assert.equal(again.tokenStatus(id(o3.tx)), 'consistent')
})

test('a member named __proto__ is a member no class defines: refused, never dropped', async () => {
  const s = await setup()
  const org = '03baf7689c0a3558fb604589036a8d1e4b685d909f6e0e2c6018a14049ae64ec26'
  const q = `{"hall":"${hall}","org":"${org}","__proto__":"x"}`
  for (const form of [q, JSON.parse(q) as unknown]) {
    let got = 'parsed'
    try {
      parseQuery(form)
    } catch (e) {
      got = reasonOf(e)
    }
    assert.equal(got, 'query', typeof form)
  }
  await assert.rejects(s.engine.lookup({ service: LookupService, query: JSON.parse(q) as unknown }), /no class has these members/)
  // The members a question does carry are its own, whatever they are named.
  assert.deepEqual(Object.keys(parseQuery(`{"hall":"${hall}","groupsOf":"${org}"}`).members).sort(), ['groupsOf', 'hall'])
})

test('a sweep admitted in two halls answers in both', async () => {
  const chain = new Chain(94000)
  const hall2 = 'other_hall_abcdefghij'
  const topic2 = `tm_borg_${hall2}`
  const m = borgModule(new FakeHost(), { ...config, halls: [hall, hall2] }, [topic, topic2], new MemoryJournal(), chain.tracker)
  const storage = new MemoryStorage()
  await m.ls.restore([], storage)
  const org = new OrgSim(hall, 'two hall org', chain)
  const a = await org.grant(new Party('two hall a'))
  // The second hall's carrier on the same tree: the funding key does not name a hall.
  const vout = org.nextGrantVout()
  const b = await carrier(org.grantTree, vout, org.party, KeyGrant, encodeGrant(await issueGrant(hall2, org.party, new Party('two hall b'), org.grantTree, vout)))
  const admit = async (t: string, tx: Transaction): Promise<void> => await m.ls.outputAdmittedByTopic({ mode: 'whole-tx', topic: t, atomicBEEF: tx.toAtomicBEEF(), outputIndex: 0 })
  await admit(topic, a.tx)
  await admit(topic2, b)
  const sw = await sweep(org.grantTree, [a.vout, vout], org.party, KeyGrant, chain)
  await admit(topic, sw)
  await admit(topic2, sw)
  const tree = id(org.grantTree)
  const asked = async (h: string, op: string): Promise<string[]> => (await m.ls.lookup({ service: LookupService, query: { hall: h, spent: op } })).map((f) => f.txid)
  assert.deepEqual([m.ls.status(toHex(a.c)), m.ls.status(cHex(b))], ['retracted', 'retracted'])
  assert.deepEqual(await asked(hall, `${tree}.${a.vout}`), [id(sw)])
  assert.deepEqual(await asked(hall2, `${tree}.${vout}`), [id(sw)], 'the hall whose carrier it retracts')
  assert.equal(m.ls.sweepCount, 1)
  // Dropped from one topic, it still answers in the other.
  m.ls.outputNoLongerRetainedInHistory(id(sw), 0, topic)
  assert.deepEqual([await asked(hall, `${tree}.${a.vout}`), await asked(hall2, `${tree}.${vout}`)], [[], [id(sw)]])
  assert.equal(m.ls.sweepCount, 1)
})

test('what the index drops is deleted from storage at once, and a store that cannot delete does not grow a queue', async () => {
  const chain = new Chain(97500)
  const s = await setup({ chain })
  const many = await equivocations(new OrgSim(hall, 'delete org', chain), 'delete', 11)
  for (const x of many) assert.deepEqual(await submit(s.engine, x.toAtomicBEEF()), [0])
  // Nine kept, two dropped, with no retention pass in between.
  const stored = (await s.storage.findUTXOsForTopic(topic)).filter((o) => many.some((x) => id(x) === o.txid)).length
  assert.deepEqual([s.ls.wrapCount, stored, s.ls.pendingDeletes], [9, 9, 0])
  for (const x of many.slice(9)) assert.equal(await s.storage.findOutput(id(x), 0, topic), null)
  // A store with no deleteOutput keeps what was dropped; the module does not keep a list of it.
  const chain2 = new Chain(98000)
  const more = await equivocations(new OrgSim(hall, 'delete org b', chain2), 'delete b', 31)
  const store = new MemoryStorage()
  const cannot = new Proxy(store, { get: (t, k) => (k === 'deleteOutput' ? undefined : Reflect.get(t, k)) }) as MemoryStorage
  const host = new FakeHost()
  const ls = borgModule(host, config, [topic], new MemoryJournal(), chain2.tracker).ls
  await ls.restore([], cannot)
  for (const x of more) await ls.outputAdmittedByTopic({ mode: 'whole-tx', topic, atomicBEEF: x.toAtomicBEEF(), outputIndex: 0 })
  await ls.prune()
  assert.deepEqual([ls.wrapCount, ls.pendingDeletes], [9, 0])
  assert.equal(host.lines.filter((l) => l.msg.includes('cannot delete dropped outputs')).length, 1, 'said once')
})

test('an admission that meets a restore in flight waits for it, and is decided as any other', async () => {
  const chain = new Chain(98500)
  const s = await setup({ chain })
  const org = new OrgSim(hall, 'in flight org', chain)
  const many = await equivocations(org, 'in flight', 10)
  for (const x of many.slice(1)) assert.deepEqual(await submit(s.engine, x.toAtomicBEEF()), [0])
  // A chain of two tokens, and a journal that does not name them: the restore walks back from the second to the first.
  const [o0, o1] = [await org.orgToken([]), await org.orgToken([])]
  for (const m of [o0, o1]) assert.deepEqual(await submit(s.engine, m.beef), [0])
  const journal = new MemoryJournal()
  for (const l of s.journal.lines) if (l.k !== 't') journal.append(l)
  const ls = borgModule(new FakeHost(), config, [topic], journal, chain.tracker).ls
  // While it reads the first token, the lowest commitment of all is admitted on the full row.
  let admitted: Promise<void> | undefined
  let seenAtOnce: unknown = 'not asked'
  const meanwhile = new Proxy(s.storage, {
    get: (t, k) =>
      k === 'findOutput'
        ? async (...args: Parameters<MemoryStorage['findOutput']>): Promise<Output | null> => {
            if (args[0] === id(o0.tx) && admitted === undefined) {
              admitted = Promise.resolve(ls.outputAdmittedByTopic({ mode: 'whole-tx', topic, atomicBEEF: many[0]!.toAtomicBEEF(), outputIndex: 0 }))
              seenAtOnce = ls.status(cHex(many[0]!))
              await new Promise((resolve) => setTimeout(resolve, 20))
            }
            return await t.findOutput(...args)
          }
        : Reflect.get(t, k),
  }) as MemoryStorage
  await ls.restore(await s.storage.findUTXOsForTopic(topic, undefined, undefined, true), meanwhile)
  assert.notEqual(admitted, undefined, 'the admission came during the restore')
  assert.equal(seenAtOnce, undefined, 'not indexed while the restore was in flight')
  await admitted
  assert.equal(ls.status(cHex(many[0]!)), 'held', 'the lowest ever admitted is the winner')
  assert.equal(ls.status(cHex(many[1]!)), 'superseded')
  assert.ok(ls.dropped(cHex(many[9]!)), 'the row keeps eight superseded: the highest went')
  assert.equal(ls.wrapCount, 9)
  assert.deepEqual([ls.tokenStatus(id(o0.tx)), ls.tokenStatus(id(o1.tx))], ['consistent', 'consistent'])
})
