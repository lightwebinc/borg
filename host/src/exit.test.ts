/**
 * The exit criterion of this application, shown through the module's
 * lookup answers on the real engine: a removal from a group rotates the
 * epoch, every host refuses a removal that does not, and the removed member
 * cannot open the next epoch's key, while the members who remain open it
 * with nothing but what the host answers. The member's checks are spec
 * section 6.4's, in its order: token, entry, undecryptable, commitment.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import type { Engine } from '@bsv/overlay'
import { Transaction } from '@bsv/sdk'
import { readToken } from './admit.js'
import { KeyRecord, LookupService, Refusal, decodeWrap, encodeWrap, recipients, type Group } from './borgrec.js'
import { checkEpochToken, commitmentOf, unwrap } from './epoch.js'
import { firstPush } from './script.js'
import { answer, hall, setup, submit } from './testhost.js'
import { Chain, OrgSim, Party, carrier, epochKey, sha, wrapEntry } from './testmint.js'
import { toHex } from './util.js'

/** Outputs of a question, as transactions. */
async function ask(e: Engine, query: Record<string, string>): Promise<Array<{ tx: Transaction; vout: number }>> {
  const a = await e.lookup({ service: LookupService, query })
  if (a.type !== 'output-list') throw new Error('not an output list')
  return a.outputs.map((o) => ({ tx: Transaction.fromBEEF(o.beef), vout: o.outputIndex }))
}

/**
 * What a member does to read its group's current epoch key from a host:
 * the latest token, the token that began its epoch (checked against the
 * wrap's epoch id), its own wrap, the decryption, the commitment.
 */
async function openEpoch(e: Engine, org: OrgSim, groupHex: string, member: Party): Promise<{ epoch: number; key?: Uint8Array; refused?: string }> {
  const latest = await ask(e, { hall, org: org.hex, group: groupHex })
  assert.equal(latest.length, 1)
  const head = readToken(latest[0]!.tx).group!
  const began = await ask(e, { hall, org: org.hex, group: groupHex, seq: String(head.epochSeq) })
  const record = readToken(began[0]!.tx).record
  const wraps = await ask(e, { hall, org: org.hex, group: groupHex, epoch: String(head.epoch), member: member.hex })
  let refused = 'entry'
  for (const w of wraps) {
    const rec = decodeWrap(firstPush(Uint8Array.from(w.tx.outputs[0]!.lockingScript.toBinary()))!)
    let g: Group
    try {
      g = checkEpochToken(record, rec)
    } catch (err) {
      refused = (err as Refusal).reason
      continue
    }
    const i = recipients(rec).findIndex((r) => toHex(r) === member.hex)
    if (i < 0) continue
    try {
      return { epoch: head.epoch, key: await unwrap(member.wallet, rec.entries[i]!, rec.org, rec.epochId, rec.epoch, g.commitment) }
    } catch (err) {
      refused = (err as Refusal).reason
    }
  }
  return { epoch: head.epoch, refused }
}

test('removal rotates the epoch, and the removed member cannot open the next one: every check through the host\'s answers', async () => {
  const chain = new Chain(90000)
  const s = await setup({ chain })
  const org = new OrgSim(hall, 'exit org', chain)
  const [alice, bob, carol] = [new Party('exit alice'), new Party('exit bob'), new Party('exit carol')]
  const grants = [await org.grant(alice), await org.grant(bob), await org.grant(carol)]
  for (const g of grants) assert.deepEqual(await submit(s.engine, g.tx.toAtomicBEEF()), [0])
  assert.deepEqual(await submit(s.engine, (await org.orgToken(grants.map((g) => g.c))).beef), [0])
  const group = await org.group('private', [alice, bob, carol])
  assert.deepEqual(await submit(s.engine, group.tip.beef), [0])
  for (const w of await group.wraps()) assert.deepEqual(await submit(s.engine, w.toAtomicBEEF()), [0])

  // Epoch 0: all three open it, and it is the key the organization sampled.
  const k0 = group.epochs.get(0)!.key
  for (const p of [alice, bob, carol]) assert.deepEqual(await openEpoch(s.engine, org, group.idHex, p), { epoch: 0, key: k0 }, p.label)

  // A removal that keeps the epoch is refused by the host, raising: nothing recorded.
  const saved = { tip: group.tip, members: group.members, history: group.history.length }
  const unrotated = await group.change({ remove: [bob], keepEpoch: true })
  assert.deepEqual(await submit(s.engine, unrotated.beef), [])
  assert.equal(s.host.count('borg_refused_total', { reason: 'rotation' }), 1)
  assert.equal(await s.storage.doesAppliedTransactionExist({ txid: unrotated.tx.id('hex'), topic: `tm_borg_${hall}` }), false)
  group.tip = saved.tip
  group.members = saved.members
  group.history.length = saved.history

  // The removal as the tooling builds it: a new epoch, a fresh key, wraps to those who remain.
  const removal = await group.change({ remove: [bob] })
  assert.equal(removal.rec.epoch, 1)
  assert.equal(removal.rec.epochSeq, removal.rec.seq)
  assert.notDeepEqual(removal.rec.commitment, commitmentOf(k0))
  assert.deepEqual(await submit(s.engine, removal.beef), [0])
  for (const w of await group.wraps()) assert.deepEqual(await submit(s.engine, w.toAtomicBEEF()), [0])
  assert.equal(s.ls.tokenStatus(removal.tx.id('hex')), 'consistent')

  // What the host answers: bob is in no group and no wrap of epoch 1 names him.
  assert.deepEqual(await answer(s.engine, { hall, org: org.hex, memberOf: bob.hex }), [])
  assert.deepEqual(await answer(s.engine, { hall, org: org.hex, memberOf: alice.hex }), [removal.tx.id('hex')])
  assert.deepEqual(await answer(s.engine, { hall, org: org.hex, group: group.idHex, epoch: '1', member: bob.hex }), [])

  // Alice and carol open epoch 1; bob is refused at the entry check.
  const k1 = group.epochs.get(1)!.key
  for (const p of [alice, carol]) assert.deepEqual(await openEpoch(s.engine, org, group.idHex, p), { epoch: 1, key: k1 }, p.label)
  assert.deepEqual(await openEpoch(s.engine, org, group.idHex, bob), { epoch: 1, refused: 'entry' })

  // Bob tries every entry of every wrap of epoch 1 with his own wallet: none opens.
  const all = await ask(s.engine, { hall, org: org.hex, group: group.idHex, epoch: '1' })
  assert.ok(all.length > 0)
  let opened = 0
  for (const w of all) {
    const rec = decodeWrap(firstPush(Uint8Array.from(w.tx.outputs[0]!.lockingScript.toBinary()))!)
    for (const entry of rec.entries) {
      try {
        await bob.wallet.decrypt({ ciphertext: Array.from(entry.subarray(102)), protocolID: [2, 'keyed content release'], keyID: Buffer.from(entry.subarray(70, 102)).toString('base64'), counterparty: org.hex })
        opened++
      } catch {
        // not his
      }
    }
  }
  assert.equal(opened, 0)
  // The key bob holds is epoch 0's, which does not meet epoch 1's commitment.
  assert.notDeepEqual(commitmentOf(k0), removal.rec.commitment)
  // Epoch 0 stays open to him: rotation is not retroactive (spec section 8).
  const old = await ask(s.engine, { hall, org: org.hex, group: group.idHex, epoch: '0', member: bob.hex })
  assert.equal(old.length, 1)

  // A later rotation: bob still cannot open it, the others can.
  const rot = await group.change({ rotate: true })
  assert.deepEqual(await submit(s.engine, rot.beef), [0])
  for (const w of await group.wraps()) assert.deepEqual(await submit(s.engine, w.toAtomicBEEF()), [0])
  assert.deepEqual(await openEpoch(s.engine, org, group.idHex, carol), { epoch: 2, key: group.epochs.get(2)!.key })
  assert.deepEqual(await openEpoch(s.engine, org, group.idHex, bob), { epoch: 2, refused: 'entry' })
})

test("a member's checks refuse a wrap the organization made wrong, each for its own reason, from the host's answers", async () => {
  const chain = new Chain(90500)
  const s = await setup({ chain })
  const org = new OrgSim(hall, 'careless org', chain)
  const [alice, carol, dave] = [new Party('careless alice'), new Party('careless carol'), new Party('careless dave')]
  const group = await org.group('careless', [alice, carol, dave])
  assert.deepEqual(await submit(s.engine, group.tip.beef), [0])
  const { epochId, key } = group.epochs.get(0)!
  const seal = async (entries: Uint8Array[], id = epochId): Promise<void> => {
    const rec = encodeWrap({ hall, org: org.party.id, group: group.id, epoch: 0, epochId: id, entries, extra: [] })
    assert.deepEqual(await submit(s.engine, (await carrier(org.recordTree, org.nextRecordVout(), org.party, KeyRecord, rec)).toAtomicBEEF()), [0], 'the host cannot tell')
  }
  // alice: another key than the one committed to.
  await seal([wrapEntry(org.party, alice.id, epochId, 0, epochKey())])
  // carol: her key, its ciphertext damaged.
  const damaged = wrapEntry(org.party, carol.id, epochId, 0, key)
  damaged[120]! ^= 1
  await seal([damaged])
  // dave: a wrap naming another epoch id than the token that began the epoch.
  const other = sha('another epoch id')
  await seal([wrapEntry(org.party, dave.id, other, 0, key)], other)
  assert.deepEqual(await openEpoch(s.engine, org, group.idHex, alice), { epoch: 0, refused: 'commitment' })
  assert.deepEqual(await openEpoch(s.engine, org, group.idHex, carol), { epoch: 0, refused: 'undecryptable' })
  assert.deepEqual(await openEpoch(s.engine, org, group.idHex, dave), { epoch: 0, refused: 'token' })
})
