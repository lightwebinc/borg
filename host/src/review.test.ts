/**
 * The contract review's attacks, replayed against the module on the real
 * engine. Spec section 11.1 (the parent in the token's BEEF, merged paths,
 * what is never admitted) and 11.2 (pending, the bounded spend table, one
 * answered carrier per outpoint) are attacked here; the rest are where
 * their own subject is tested:
 *
 *   suppression by a worse BEEF, any object    every pair below
 *   headers that lag a token's parent          engine.test.ts
 *   pending answers from a complete fold       ls_borg.test.ts
 *   a sweep before its carriers; a stranger's
 *   chain of sweeps (bounded table)            ls_borg.test.ts
 *   evidence kept per outpoint                 ls_borg.test.ts, engine.test.ts
 *   replay of stored tokens to a sibling       engine.test.ts
 *   the roll answers a grant whatever its
 *   status; the member answer's path          ls_borg.test.ts, engine.test.ts
 *   a priced spelling of a free question      ls_borg.test.ts
 *   removal without rotation                  exit.test.ts
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { MemoryJournal } from './journal.js'
import { BorgLookupService } from './ls_borg.js'
import { hall, setup, submit, topic } from './testhost.js'
import { Chain, OrgSim } from './testmint.js'
import { tokenBEEF } from './beef.js'
import { FakeHost, beefOf, byName, txVectors } from './testutil.js'
import { Transaction } from '@bsv/sdk'

const v = txVectors()

test('suppression, every way the vectors make it: each refused BEEF of a txid offered first, the genuine one admitted after it', async () => {
  const admitted = new Map(v.cases.filter((t) => t.verdict === 'admit').map((t) => [t.txid, t]))
  let pairs = 0
  // A case that names its own sweep bound is refused only under that bound.
  for (const worse of v.cases.filter((t) => t.verdict === 'refuse' && admitted.has(t.txid) && t.maxSweepInputs === undefined)) {
    const genuine = admitted.get(worse.txid)!
    const s = await setup()
    const got = await submit(s.engine, beefOf(worse))
    assert.ok(got === undefined || got.length === 0, worse.name)
    assert.equal(await s.storage.doesAppliedTransactionExist({ txid: worse.txid, topic }), false, worse.name)
    assert.deepEqual(await submit(s.engine, beefOf(genuine)), genuine.admits!.outputsToAdmit, `${worse.name} then ${genuine.name}`)
    pairs++
  }
  assert.equal(pairs, 18)
})

test('a token mined in the block of its parent: the one merged path is admitted through the engine', async () => {
  const chain = new Chain(91000)
  const s = await setup({ chain })
  const org = new OrgSim(hall, 'same block org', chain)
  const o0 = await org.orgToken([], [], { mine: false })
  const o1 = await org.orgToken([], [], { mine: false })
  chain.mineTogether(o0.tx, o1.tx)
  // The chain tree was mined in an earlier block; seq 0 and seq 1 share one.
  assert.deepEqual(await submit(s.engine, tokenBEEF(o0.tx, org.chainTree)), [0])
  const merged = tokenBEEF(o1.tx, o0.tx)
  assert.deepEqual(await submit(s.engine, merged), [0])
  assert.equal(s.ls.tokenStatus(o1.tx.id('hex')), 'consistent')
})

test('a transaction that only spends held outputs is refused not-borg, and the held output is not marked spent', async () => {
  const s = await setup()
  assert.deepEqual(await submit(s.engine, beefOf(byName(v, 'sweep'))), [0])
  const spend = byName(v, 'spend-held-tombstone')
  assert.deepEqual(await submit(s.engine, beefOf(spend)), [])
  assert.equal(s.host.count('borg_refused_total', { reason: 'not-borg' }), 1)
  assert.equal(await s.storage.doesAppliedTransactionExist({ txid: spend.txid, topic }), false)
  assert.equal((await s.storage.findOutput(byName(v, 'sweep').txid, 0, topic))?.spent, false)
})

test('a mined funding tree submitted alone: output 0 only, its spend in the bounded table, nothing journaled for life', async () => {
  const s = await setup()
  const tree = byName(v, 'funding-tree-alone')
  assert.deepEqual(await submit(s.engine, beefOf(tree)), [0])
  assert.equal((await s.storage.findOutputsForTransaction(tree.txid)).length, 1, 'one output held')
  assert.equal(s.ls.tableSize, 1)
  assert.equal(s.ls.rowCount, 0)
  assert.ok(!s.journal.lines.some((l) => l.k === 's'))
})

test('a flood of tokens at one seq: the lowest eight txids are kept, whatever order they arrive in', async () => {
  const chain = new Chain(92000)
  const org = new OrgSim(hall, 'fork flood org', chain)
  const g0 = await org.orgToken([])
  const forks: Transaction[] = []
  const beefs: Uint8Array[] = []
  for (let i = 0; i < 10; i++) {
    org.tip = g0
    org.roll = []
    const m = await org.orgToken([], [], { mutate: (r) => (r.name = `fork ${i}`) })
    forks.push(m.tx)
    beefs.push(m.beef)
  }
  const want = forks.map((t) => t.id('hex')).sort().slice(0, 8)
  const latest = async (order: number[]): Promise<string[]> => {
    const s = await setup({ chain })
    await submit(s.engine, g0.beef)
    for (const i of order) assert.deepEqual(await submit(s.engine, beefs[i]!), [0])
    return (await s.ls.lookup({ service: 'ls_borg', query: { hall, org: org.hex } })).map((f) => f.txid).sort()
  }
  assert.deepEqual(await latest([0, 1, 2, 3, 4, 5, 6, 7, 8, 9]), want)
  assert.deepEqual(await latest([9, 8, 7, 6, 5, 4, 3, 2, 1, 0]), want)
  // A restored index keeps the same eight from storage, which holds all ten.
  const s = await setup({ chain })
  await submit(s.engine, g0.beef)
  for (const b of beefs) await submit(s.engine, b)
  const again = new BorgLookupService({ halls: new Set([hall]), host: new FakeHost(), journal: s.journal as MemoryJournal })
  await again.restore(await s.storage.findUTXOsForTopic(topic, undefined, undefined, true), s.storage)
  assert.deepEqual((await again.lookup({ service: 'ls_borg', query: { hall, org: org.hex } })).map((f) => f.txid).sort(), want)
})
