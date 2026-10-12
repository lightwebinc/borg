/**
 * Every Go-generated vector under testdata/vectors, run through the
 * TypeScript codec: the records decode and re-encode byte for byte, refuse
 * for the same reasons, the roots, keys and wraps reproduce, and every
 * transaction case is admitted or refused exactly as the Go codec decided.
 */
import assert from 'node:assert/strict'
import { createCipheriv } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import { KeyDeriver, PrivateKey, ProtoWallet, SymmetricKey, type ChainTracker } from '@bsv/sdk'
import { readerLockingKey } from '@lightwebinc/bcommon'
import {
  AnyoneKey,
  GrantTypeString,
  MagicGrant,
  MagicGroup,
  MagicOrg,
  MagicWrap,
  Protocol,
  ProtocolName,
  ReleaseProtocol,
  TagFunding,
  TagGroup,
  TagOrg,
  WrapVersion,
  checkGroupTransition,
  checkOrgTransition,
  decodeGrant,
  decodeGroup,
  allows,
  decodeOrg,
  decodePolicy,
  effective,
  decodeWrap,
  encodeGrant,
  encodeGroup,
  encodeOrg,
  encodePolicy,
  encodeWrap,
  grantTypeBase64,
  newHall,
  parseEntry,
  reasonOf,
  releaseKeyID,
  topic,
} from './borgrec.js'
import { admit, readToken, storedToken, seqOf, type Admission } from './admit.js'
import { tokenBEEF } from './beef.js'
import { GroupFold, OrgFold, carrierStatus, winner, type TokenStatus } from './chain.js'
import { checkCertificate, checkEpochToken, checkReveal, commitmentOf, epochID, openFields, preimage, releaseInvoice, releaseKeyIDString, symmetricKey, unwrap } from './epoch.js'
import { decodeContext, encodeContext, membersRoot, policyRoot, rollRoot, verifyGrant, verifyPolicy } from './merkle.js'
import { asked, checkRoleName, grantsAny, grantsPermission, intersect, parsePermission, write as writePermission } from './permission.js'
import { parseQuery } from './query.js'
import { checkFieldValue, grants, isMembership, parseExpiry, parseScope } from './scope.js'
import { classify } from './script.js'
import { concat, fromHex, toHex } from './util.js'

const dir = new URL('../../testdata/vectors/', import.meta.url)
// eslint-disable-next-line @typescript-eslint/no-explicit-any
const load = (name: string): any => JSON.parse(readFileSync(new URL(name, dir), 'utf8'))
const hex = (b: Uint8Array): string => toHex(b)
const reason = (f: () => unknown): string => {
  try {
    f()
  } catch (e) {
    return reasonOf(e)
  }
  return 'ok'
}
const reasonAsync = async (f: () => Promise<unknown>): Promise<string> => {
  try {
    await f()
  } catch (e) {
    return reasonOf(e)
  }
  return 'ok'
}

const derivation = load('derivation-v1.json')
const privOf = (party: string): PrivateKey => PrivateKey.fromHex(derivation.identities.find((p: { party: string }) => p.party === party).privateKey)
const pubOf = (party: string): Uint8Array => Uint8Array.from(privOf(party).toPublicKey().encode(true) as number[])

test('derivation-v1: the registry values and every derived key', () => {
  const r = derivation.registry
  assert.deepEqual(r.protocol, [1, ProtocolName])
  assert.deepEqual(r.releaseProtocol, ReleaseProtocol)
  assert.equal(r.tagOrg, hex(TagOrg))
  assert.equal(r.tagFunding, hex(TagFunding))
  assert.equal(r.tagGroup, hex(TagGroup))
  assert.equal(r.magicOrg, hex(MagicOrg))
  assert.equal(r.magicGroup, hex(MagicGroup))
  assert.equal(r.magicGrant, hex(MagicGrant))
  assert.equal(r.magicWrap, hex(MagicWrap))
  assert.equal(r.grantTypeString, GrantTypeString)
  assert.equal(r.grantType, grantTypeBase64())
  assert.equal(r.anyoneKey, hex(AnyoneKey))
  for (const d of derivation.derivedKeys) {
    for (const k of ['org', 'group', 'record', 'grant', 'chain']) {
      assert.equal(readerLockingKey(Protocol, k, d.identityKey).toString(), d[k], `${d.party} ${k}`)
    }
  }
  const drawn = derivation.halls.drawnFromBytes
  const bytes = fromHex(drawn.bytes)
  let at = 0
  assert.equal(newHall('example_hall', (n) => bytes.subarray(at, (at += n))), drawn.result)
  assert.equal(topic(derivation.halls.vectors), derivation.halls.topic)
})

test('epoch-v1: BRC-369 reproduced, its header refused, and the scenario epochs', () => {
  const e = load('epoch-v1.json')
  const b = e.brc369
  const k = fromHex(b.key)
  assert.equal(hex(symmetricKey(k)), b.symmetricKey)
  assert.equal(hex(commitmentOf(k)), b.commitment)
  assert.equal(hex(releaseKeyID(fromHex(b.contentId), b.epoch)), b.releaseKeyId)
  assert.equal(releaseInvoice(fromHex(b.contentId), b.epoch), b.releaseInvoice)
  const payload = fromHex(b.payload)
  assert.equal(hex(payload.subarray(0, 4)), b.payloadVersion)
  assert.equal(hex(WrapVersion), b.sdkVersion)
  assert.equal(reason(() => parseEntry(payload)), 'entry')
  // The vector's "shared secret" opens the payload directly as the AES key.
  const secret = Array.from(fromHex('873de6bc082794ae1de8390aec85b7641fb7dd88106cd0a3230468a8e9484547'))
  assert.equal(hex(Uint8Array.from(new SymmetricKey(secret).decrypt(Array.from(payload.subarray(102))) as number[])), b.payloadPlaintext)
  for (const ep of e.epochs) {
    const key = fromHex(ep.key)
    assert.equal(hex(symmetricKey(key)), ep.symmetricKey)
    assert.equal(hex(commitmentOf(key)), ep.commitment)
    assert.equal(hex(releaseKeyID(fromHex(ep.epochId), ep.epoch)), ep.releaseKeyId)
    assert.equal(releaseInvoice(fromHex(ep.epochId), ep.epoch), ep.releaseInvoice)
  }
})

test('org-v1: records, roll roots, transitions and the member context', () => {
  const o = load('org-v1.json')
  for (const r of o.records) {
    const d = decodeOrg(fromHex(r.hex))
    assert.equal(hex(encodeOrg(d)), r.hex, r.label)
    assert.equal(hex(rollRoot(r.rollOf.map(fromHex))), r.roll, r.label)
  }
  const recs = o.records.map((r: { hex: string }) => decodeOrg(fromHex(r.hex)))
  const txid0 = fromHex(o.records[0].standInTxid)
  assert.equal(reason(() => checkOrgTransition(recs[o.transition.from], fromHex(o.records[o.transition.from].standInTxid), recs[o.transition.to])), 'ok')
  for (const c of o.refused) assert.equal(reason(() => checkOrgTransition(recs[0], txid0, decodeOrg(fromHex(c.to)))), c.verdict, c.label)
  const mc = o.memberContext
  const ctx = decodeContext(fromHex(mc.context))
  assert.equal(hex(encodeContext(ctx)), mc.context)
  const rollOf = o.records[mc.rollOf].roll
  assert.equal(verifyGrant(ctx, fromHex(o.commitments[mc.grant]), fromHex(rollOf)), mc.verifies)
  assert.equal(verifyGrant(ctx, fromHex(mc.refused), fromHex(rollOf)), false)
})

test('group-v1: records, transitions, folds and rejoins', () => {
  const g = load('group-v1.json')
  const recs = g.records.map((r: { hex: string }) => decodeGroup(fromHex(r.hex)))
  for (const [i, r] of g.records.entries()) {
    assert.equal(hex(encodeGroup(recs[i])), r.hex, r.label)
    assert.equal(hex(membersRoot(r.membersAfter.map(fromHex))), r.members, r.label)
    if (r.beginsEpoch) assert.equal(hex(epochID(fromHex(r.hex))), r.epochId)
  }
  const at = (s: string) => (s.length < 4 ? { rec: recs[Number(s)], txid: fromHex(g.records[Number(s)].standInTxid) } : { rec: decodeGroup(fromHex(s)), txid: undefined })
  for (const t of g.transitions) {
    const from = at(t.from)
    const to = at(t.to)
    const txid = from.txid ?? fromHex(g.records[0].standInTxid)
    const got = reason(() => checkGroupTransition(from.rec, txid, to.rec))
    assert.equal(got === 'ok' ? 'admitted' : got, t.verdict, t.label)
  }
  for (const c of g.fold.cases) {
    const f = new GroupFold()
    // Seed the fold with membersBefore by a synthetic genesis.
    const seed = { ...recs[0], add: c.membersBefore.map(fromHex), count: c.membersBefore.length, members: membersRoot(c.membersBefore.map(fromHex)) }
    assert.equal(f.apply(seed), 'consistent')
    assert.equal(f.apply(decodeGroup(fromHex(c.token))), c.verdict, c.label)
  }
  for (const c of g.rejoin.cases) {
    const f = new GroupFold()
    for (const r of recs) assert.equal(f.apply(r), 'consistent')
    const tok = decodeGroup(fromHex(c.token))
    assert.equal(reason(() => checkGroupTransition(recs[3], fromHex(g.records[3].standInTxid), tok)), 'ok', c.label)
    assert.equal(f.apply(tok), c.verdict, c.label)
  }
})

test('grant-v1: records, preimages, signatures, reveals and each member opening its own', async () => {
  const g = load('grant-v1.json')
  for (const v of g.grants) {
    const d = decodeGrant(fromHex(v.hex))
    assert.equal(hex(encodeGrant(d)), v.hex, v.label)
    assert.equal(hex(preimage(d)), v.preimage, v.label)
    await checkCertificate(d)
    if (d.reveal !== undefined) assert.deepEqual(checkReveal(d), v.values, v.label)
    const subject = derivation.derivedKeys.find((k: { identityKey: string }) => k.identityKey === v.subject).party
    assert.deepEqual(await openFields(d, new ProtoWallet(privOf(subject))), v.values, v.label)
    assert.equal(await reasonAsync(() => openFields(d, new ProtoWallet(privOf('dave')))), 'other', v.label)
    assert.equal(isMembership(v.values.scope), v.membership)
    assert.equal(grants(parseScope(v.values.scope), 'admin'), v.admin)
  }
})

/** WrapWithIV: the release under a fixed IV, for byte-for-byte comparison. */
function wrapWithIV(org: PrivateKey, member: Uint8Array, epochId: Uint8Array, epoch: number, k: Uint8Array, iv: Uint8Array): Uint8Array {
  const sk = new KeyDeriver(org).deriveSymmetricKey(ReleaseProtocol, releaseKeyIDString(epochId, epoch), toHex(member))
  const key = Uint8Array.from(sk.toArray('be', 32))
  const c = createCipheriv('aes-256-gcm', key, iv)
  const body = concat([c.update(k), c.final(), c.getAuthTag()])
  return concat([WrapVersion, pubOf('org'), member, releaseKeyID(epochId, epoch), iv, body])
}

test('wrap-v1: every wrap reproduced and opened by exactly its members', async () => {
  const w = load('wrap-v1.json')
  const e = load('epoch-v1.json')
  const g = load('group-v1.json')
  for (const v of w.wraps) {
    const d = decodeWrap(fromHex(v.hex))
    assert.equal(hex(encodeWrap(d)), v.hex, v.label)
    const key = fromHex(e.epochs[v.epoch].key)
    for (const ent of v.entries) {
      assert.equal(hex(wrapWithIV(privOf('org'), fromHex(ent.recipient), d.epochId, d.epoch, key, fromHex(ent.iv))), ent.wrap, v.label)
    }
    const began = g.records.find((r: { beginsEpoch: boolean; epoch: number }) => r.beginsEpoch && r.epoch === v.epoch)
    checkEpochToken(fromHex(began.hex), d)
    for (const o of v.opens) {
      let got: Uint8Array | undefined
      for (const ent of d.entries) {
        try {
          got = await unwrap(new ProtoWallet(privOf(o.party)), ent, d.org, d.epochId, d.epoch, fromHex(began.commitment))
        } catch {
          continue
        }
      }
      assert.equal(got !== undefined, o.opens, `${v.label}: ${o.party}`)
      if (got !== undefined) assert.equal(hex(got), o.key)
    }
  }
  // A wrap held against another epoch's token fails check 1.
  const later = decodeWrap(fromHex(w.wraps[2].hex))
  assert.equal(reason(() => checkEpochToken(fromHex(g.records[0].hex), later)), 'token')
})

test('scope-v1: the grammar and its fail-closed meaning', () => {
  const s = load('scope-v1.json')
  for (const c of s.scopes) {
    assert.equal(reason(() => parseScope(c.scope)) === 'ok', c.parses, c.scope)
    assert.equal(isMembership(c.scope), c.membership, c.scope)
    if (c.parses) assert.equal(grants(parseScope(c.scope), 'admin'), c.admin, c.scope)
  }
  for (const c of s.expiry) assert.equal(reason(() => checkFieldValue('expiry', new TextEncoder().encode(c.value))) === 'ok', c.ok, c.value)
  for (const c of s.roles) assert.equal(reason(() => checkFieldValue('role', fromHex(c.hex))) === 'ok', c.ok, c.hex)
})

test('refusal-v1: every record refused for the same reason, unknown keys kept', async () => {
  const f = load('refusal-v1.json')
  const dec: Record<string, (b: Uint8Array) => unknown> = { org: decodeOrg, group: decodeGroup, grant: decodeGrant, wrap: decodeWrap, policy: decodePolicy }
  for (const r of f.refusals) {
    if (r.stage === 'record') {
      assert.equal(reason(() => dec[r.record]!(fromHex(r.hex))), r.reason, `${r.record}: ${r.label}`)
    } else if (r.reason === 'grant-signature') {
      assert.equal(await reasonAsync(() => checkCertificate(decodeGrant(fromHex(r.hex)))), r.reason, r.label)
    } else if (r.reason === 'grant-reveal') {
      assert.equal(reason(() => checkReveal(decodeGrant(fromHex(r.hex)))), r.reason, r.label)
    }
  }
  const k = f.unknownKeyKept
  const enc: Record<string, (b: Uint8Array) => Uint8Array> = {
    org: (b) => encodeOrg(decodeOrg(b)),
    group: (b) => encodeGroup(decodeGroup(b)),
    grant: (b) => encodeGrant(decodeGrant(b)),
    wrap: (b) => encodeWrap(decodeWrap(b)),
  }
  assert.equal(hex(enc[k.record]!(fromHex(k.hex))), k.hex)
})

test('permission-v1: the grammar, matching, role names and attenuation', () => {
  const f = load('permission-v1.json')
  for (const c of f.permissions) {
    const r = reason(() => parsePermission(c.value))
    assert.equal(r === 'ok', c.parses, `${c.value}: ${c.why}`)
    if (!c.parses) {
      assert.equal(r, 'permission', c.value)
      continue
    }
    const p = parsePermission(c.value)
    assert.deepEqual({ service: p.service, action: p.action, resource: p.resource, prefix: p.prefix },
      { service: c.service, action: c.action, resource: c.resource, prefix: c.prefix }, c.value)
    assert.equal(writePermission(p), c.value, c.value)
  }
  for (const m of f.matches) {
    assert.equal(grantsPermission(parsePermission(m.held), asked(m.asked)), m.grants, `${m.held} grants ${m.asked}: ${m.why}`)
    assert.equal(grantsAny([m.held], m.asked), m.grants, `${m.held} grants ${m.asked}`)
  }
  for (const a of f.asked) assert.equal(reason(() => asked(a.value)) === 'ok', a.ok, `${a.value}: ${a.why}`)
  for (const r of f.roleNames) {
    const got = reason(() => checkRoleName(r.value))
    assert.equal(got === 'ok', r.ok, `${r.value}: ${r.why}`)
    if (!r.ok) assert.equal(got, 'role', r.value)
  }
  for (const i of f.intersect) assert.deepEqual(intersect(i.role, i.keep ?? []), i.kept, i.why)
})

test('org-v1: succession, announced one token ahead and refused otherwise', () => {
  const f = load('org-v1.json').succession
  const announce = decodeOrg(fromHex(f.announce))
  const handover = decodeOrg(fromHex(f.handover))
  assert.equal(hex(announce.successor!), f.successor, 'the announcing token names the successor')
  assert.equal(hex(handover.org), f.successor, 'the next token carries that key')
  const txid = fromHex(load('org-v1.json').records[0].standInTxid)
  checkOrgTransition(announce, txid, handover)
  // A predecessor that names nobody, and a handover to another key.
  assert.equal(reason(() => checkOrgTransition(decodeOrg(fromHex(load('org-v1.json').records[0].hex)), txid, handover)), f.unannounced.reason)
  assert.equal(reason(() => checkOrgTransition(announce, txid, decodeOrg(fromHex(f.otherKey.hex)))), f.otherKey.reason)
})

test('policy-v1: records, the policy set and its root, and a context into it', () => {
  const f = load('policy-v1.json')
  for (const r of f.records) {
    const p = decodePolicy(fromHex(r.hex))
    assert.equal(p.kind, r.kind, r.name)
    assert.equal(p.role, r.role, r.name)
    assert.deepEqual(p.permissions, r.permissions, r.name)
    assert.equal(p.expiry, r.expiry ?? null, r.name)
    assert.equal(p.to === null ? '' : hex(p.to), r.to ?? '', r.name)
    if (r.toKind !== undefined) assert.equal(p.to!.length === 16 ? 'group' : 'key', r.toKind, r.name)
    assert.equal(hex(encodePolicy(p)), r.hex, `${r.name} re-encodes`)
  }
  let prev: ReturnType<typeof decodeOrg> | undefined
  for (const c of f.chain) {
    const o = decodeOrg(fromHex(c.hex))
    assert.equal(o.seq, c.seq)
    assert.deepEqual(o.define.map(hex), c.define, `seq ${c.seq} define`)
    assert.deepEqual(o.retire.map(hex), c.retire, `seq ${c.seq} retire`)
    assert.equal(o.policyCount, c.set.length, `seq ${c.seq} policyCount`)
    assert.equal(hex(policyRoot(c.set.map(fromHex))), c.policy, `seq ${c.seq} policy root`)
    assert.equal(hex(o.policy), c.policy, `seq ${c.seq} record root`)
    if (prev !== undefined) checkOrgTransition(prev, o.prev, o)
    prev = o
  }
  const ctx = decodeContext(fromHex(f.context.hex))
  assert.equal(verifyPolicy(ctx, fromHex(f.context.of), fromHex(f.context.root)), true, 'the context verifies against its own root')
  assert.equal(verifyPolicy(ctx, fromHex(f.context.of), fromHex(f.context.wrongRoot)), false, 'and against no other')
})

test('authz-v1: the effective permissions of each subject, and what each may do', () => {
  const f = load('authz-v1.json')
  const byName = new Map<string, ReturnType<typeof decodePolicy>>()
  for (const r of load('policy-v1.json').records) byName.set(r.name, decodePolicy(fromHex(r.hex)))
  const set = f.set.map((n: string) => byName.get(n)!)
  const now = parseExpiry(f.now)
  for (const s of f.subjects) {
    const subject = {
      key: fromHex(s.key),
      member: s.member,
      roles: s.roles,
      groups: s.groups.map(fromHex),
    }
    const eff = effective(subject, set, now)
    assert.deepEqual(eff, s.effective, `${s.name}: ${s.why}`)
    for (const a of s.asked) assert.equal(allows(eff, a.permission), a.allowed, `${s.name} asks ${a.permission}`)
  }
})

test('query-v1: every question classed or refused', () => {
  const q = load('query-v1.json')
  for (const c of q.questions) {
    let got: string
    try {
      got = parseQuery(c.question).class.name
    } catch {
      got = 'refused'
    }
    assert.equal(got, c.refused ? 'refused' : c.class, c.question)
  }
})

test('script-v1: the classifier', () => {
  const s = load('script-v1.json')
  for (const c of s.scripts) assert.equal(classify(fromHex(c.script)).replace(/^none$/, 'none'), c.claims, c.label)
})

/** The test chain's headers as a chain tracker: merkle roots in display order. */
function tracker(headers: Array<{ height: number; merkleRoot: string }>): ChainTracker {
  const m = new Map(headers.map((h) => [h.height, h.merkleRoot]))
  return {
    isValidRootForHeight: async (root: string, height: number) => m.get(height) === root,
    currentHeight: async () => Math.max(...m.keys()),
  }
}

function admissionJSON(a: Admission): Record<string, unknown> {
  const j: Record<string, unknown> = { kind: a.kind, outputsToAdmit: a.outputs, coinsToRetain: a.retain }
  if (a.carrier !== undefined) {
    j.commitment = hex(a.carrier.commitment)
    j.record = hex(a.carrier.record)
    j.owner = hex(a.carrier.owner)
    j.fundingTxid = a.carrier.funding.txid
    j.fundingVout = a.carrier.funding.vout
  }
  if (a.token !== undefined) {
    j.seq = seqOf(a.token)
    j.record = hex(a.token.record)
    j.parentTxid = a.parent
    if (a.predecessor !== undefined) j.predecessorSeq = seqOf(a.predecessor)
  }
  if (a.spent !== undefined) j.spent = a.spent.map((o) => `${o.txid}.${o.vout}`)
  return j
}

test('transaction-v1: every case admitted or refused as the Go codec decided', async () => {
  const f = load('transaction-v1.json')
  const headers = tracker(f.headers)
  assert.ok(f.cases.length >= 90)
  for (const c of f.cases) {
    const held = c.previousCoins.map((p: { inputIndex: number }) => p.inputIndex)
    let a: Admission | undefined
    let got = 'admit'
    try {
      a = await admit(fromHex(c.beef), held, { hall: f.hall, headers, maxBEEF: f.maxBeef, maxSweepInputs: c.maxSweepInputs })
    } catch (e) {
      got = reasonOf(e)
    }
    if (c.verdict === 'refuse') {
      assert.equal(got, c.reason, c.name)
      continue
    }
    assert.equal(got, 'admit', c.name)
    const want = Object.fromEntries(Object.entries(c.admits).filter(([, v]) => v !== undefined))
    assert.deepEqual(admissionJSON(a!), want, c.name)
  }
})

test('transaction-v1: stored tokens read without their parents, and replayed byte for byte', async () => {
  const f = load('transaction-v1.json')
  const byTxid = new Map<string, string>(f.stored.map((s: { txid: string; stored: string }) => [s.txid, s.stored]))
  const chainTree = new Map<string, string>()
  for (const c of f.cases) if (c.name === 'org-token-genesis') chainTree.set('genesis', c.beef)
  for (const s of f.stored) {
    const { tx, token } = storedToken(fromHex(s.stored))
    assert.equal(seqOf(token), s.seq, s.name)
    assert.equal(token.kind, s.kind, s.name)
    assert.equal(hex(token.record), s.record, s.name)
    assert.equal(readToken(tx).vout, token.vout)
    const parent = byTxid.get(s.parentTxid)
    if (parent === undefined) continue // a chain tree, which no host keeps
    const p = storedToken(fromHex(parent)).tx
    assert.equal(hex(tokenBEEF(tx, p)), s.replayBeef, s.name)
  }
})

test('index-v1: folds and outpoint rows', () => {
  const f = load('index-v1.json')
  for (const c of f.broken) {
    const of = new OrgFold()
    const gf = new GroupFold()
    const got: TokenStatus[] = c.records.map((r: string) => (c.kind === 'org' ? of.apply(decodeOrg(fromHex(r))) : gf.apply(decodeGroup(fromHex(r)))))
    assert.deepEqual(got, c.statuses, c.label)
  }
  for (const r of f.outpoints) {
    const row = { commitments: r.carriers.map((c: { commitment: string }) => fromHex(c.commitment)), winnerDropped: r.winnerDropped, swept: r.swept }
    assert.equal(hex(winner(row)!), r.winner, r.label)
    for (const c of r.carriers) assert.equal(carrierStatus(row, fromHex(c.commitment)), c.status, r.label)
  }
})
