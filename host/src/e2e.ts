/**
 * The host module end to end on a local reference overlay host, loaded
 * from the bundle (bundle/borg-module.js) through OVERLAY_MODULES.
 *
 *   node dist/e2e.js /path/to/reference-host
 *
 * The reference host directory holds its compiled dist/index.js and its
 * node_modules. The bundle is staged as a deployment places it: copied into
 * a modules/ directory whose only node_modules is the host's, so it loads
 * with nothing but what the host provides, and the SDK it imports is the
 * host's own copy. The run needs Docker, for a throwaway MySQL (the host's
 * only store), and serves block headers itself in the host's native shape
 * (/v1/root/<height>, /v1/tip) for a local chain it mines on; the module
 * reads the same source through OVERLAY_CHAIN_TRACKER_URL.
 *
 * It creates an organisation, grants three members, puts them in a group,
 * wraps the epoch key to them, opens it as each member from the host's
 * answers, removes one member (a new epoch the removed member cannot open),
 * rotates, revokes the removed member's grant by the roll and by a sweep,
 * replays a suppression attempt, asks a priced class through the terms
 * route and pays its 402, restarts the host, and asks everything again of
 * the restored index. Everything it starts, it stops.
 */
import { execFileSync, spawn, type ChildProcess } from 'node:child_process'
import { copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync } from 'node:fs'
import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http'
import type { AddressInfo } from 'node:net'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { AuthFetch, PrivateKey, Transaction, type WalletInterface } from '@bsv/sdk'
import { readToken } from './admit.js'
import { TestNetwork, serveNetwork } from '@lightwebinc/bcommon/testing'
import { KeyGrant, Refusal, decodeWrap, recipients, type Group } from './borgrec.js'
import { checkEpochToken, unwrap } from './epoch.js'
import { decodeContext, verifyGrant } from './merkle.js'
import { firstPush } from './script.js'
import { Chain, OrgSim, Party, PayingWallet, sweep } from './testmint.js'
import { toHex } from './util.js'

const hall = 'example_hall_vkqzmtrwxp'
const topic = `tm_borg_${hall}`
const password = 'borg-e2e'
const payeeKey = '11'.repeat(32)
const bundle = fileURLToPath(new URL('../bundle/borg-module.js', import.meta.url))

function fail(msg: string): never {
  throw new Error(`e2e: ${msg}`)
}

const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms))

/** Stages the bundle beside the reference host's node_modules; returns the module's path and the root to remove. */
function stage(dir: string): { file: string; root: string } {
  const root = mkdtempSync(join(tmpdir(), 'borg-e2e-'))
  symlinkSync(resolve(dir, 'node_modules'), join(root, 'node_modules'))
  mkdirSync(join(root, 'modules', 'borg'), { recursive: true })
  mkdirSync(join(root, 'state'))
  const file = join(root, 'modules', 'borg', 'borg-module.js')
  copyFileSync(bundle, file)
  return { file, root }
}

/** The chain's headers, served in the reference host's native shape, read at each request. */
async function headers(chain: Chain, net: TestNetwork): Promise<Server> {
  const server = createServer((req, res) => {
    void serveNetwork(net, req, res).then((served) => {
      if (!served) headersOnly(chain, req, res)
    })
  })
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r))
  return server
}

function headersOnly(chain: Chain, req: IncomingMessage, res: ServerResponse): void {
  {
    const m = /^\/v1\/root\/(\d+)$/.exec(req.url ?? '')
    const tip = Math.max(...chain.roots.keys())
    const body = req.url === '/v1/tip' ? { height: tip } : m !== null && chain.roots.has(Number(m[1])) ? { merkleRoot: chain.roots.get(Number(m[1])) } : undefined
    res.writeHead(body === undefined ? 404 : 200, { 'content-type': 'application/json' })
    res.end(JSON.stringify(body ?? {}))
  }
}

function docker(...args: string[]): string {
  return execFileSync('docker', args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }).trim()
}

async function mysql(): Promise<{ id: string; port: number }> {
  const id = docker('run', '-d', '--rm', '-e', `MYSQL_ROOT_PASSWORD=${password}`, '-e', 'MYSQL_DATABASE=overlay', '-p', '127.0.0.1::3306', '--tmpfs', '/var/lib/mysql', 'mysql:9')
  const port = Number(docker('port', id, '3306/tcp').split('\n')[0]!.split(':').pop())
  for (let i = 0; i < 120; i++) {
    try {
      // Over TCP, which the image's initialisation server does not listen on.
      docker('exec', id, 'mysql', '-h127.0.0.1', '-uroot', `-p${password}`, '-e', 'select 1')
      return { id, port }
    } catch {
      await sleep(1000)
    }
  }
  docker('rm', '-f', id)
  fail('MySQL did not come up')
}

async function freePort(): Promise<number> {
  return await new Promise<number>((r) => {
    const s = createServer().listen(0, '127.0.0.1', () => {
      const p = (s.address() as AddressInfo).port
      s.close(() => r(p))
    })
  })
}

interface Host {
  proc: ChildProcess
  url: string
  terms: string
  out: string[]
}

interface Ports {
  db: number
  headers: number
  host: number
  terms: number
}

async function startHost(dir: string, staged: { file: string; root: string }, p: Ports): Promise<Host> {
  const out: string[] = []
  const proc = spawn(process.execPath, [resolve(dir, 'dist/index.js')], {
    env: {
      PATH: process.env['PATH'] ?? '',
      OVERLAY_TOPICS: topic,
      OVERLAY_MODULES: staged.file,
      OVERLAY_KNEX_URL: `mysql://root:${password}@127.0.0.1:${p.db}/overlay`,
      OVERLAY_CHAIN_TRACKER_URL: `http://127.0.0.1:${p.headers}`,
      OVERLAY_ADMIN_TOKEN: 'borg-e2e',
      OVERLAY_LISTEN: '127.0.0.1',
      OVERLAY_PORT: String(p.host),
      BORG_HALLS: hall,
      BORG_STATE_DIR: join(staged.root, 'state'),
      BORG_LISTEN: `127.0.0.1:${p.terms}`,
      BORG_PRICES: 'history=5,history-after=5',
      BORG_PAYEE_KEY: payeeKey,
      BORG_ARCADE_URL: `http://127.0.0.1:${p.headers}/arcade`,
      BORG_ASSET_URL: `http://127.0.0.1:${p.headers}`,
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  })
  proc.stdout!.on('data', (b: Buffer) => out.push(b.toString()))
  proc.stderr!.on('data', (b: Buffer) => out.push(b.toString()))
  const url = `http://127.0.0.1:${p.host}`
  for (let i = 0; i < 60; i++) {
    if (proc.exitCode !== null) fail(`the host exited ${proc.exitCode}:\n${out.join('')}`)
    try {
      if ((await fetch(`${url}/readyz`)).status === 200) return { proc, url, terms: `http://127.0.0.1:${p.terms}`, out }
    } catch {
      // not listening yet
    }
    await sleep(500)
  }
  proc.kill('SIGKILL')
  fail(`the host did not become ready:\n${out.join('')}`)
}

async function stopHost(h: Host): Promise<void> {
  if (h.proc.exitCode !== null) return
  const done = new Promise((r) => h.proc.once('exit', r))
  h.proc.kill('SIGTERM')
  await done
}

async function submit(h: Host, beef: number[] | Uint8Array, what: string): Promise<number[]> {
  const res = await fetch(`${h.url}/submit`, {
    method: 'POST',
    headers: { 'content-type': 'application/octet-stream', 'x-topics': topic },
    body: Uint8Array.from(beef),
  })
  const body = (await res.json()) as Record<string, { outputsToAdmit?: number[] }>
  if (res.status !== 200) fail(`submit ${what}: ${res.status} ${JSON.stringify(body)}`)
  return body[topic]?.outputsToAdmit ?? []
}

interface Out {
  tx: Transaction
  vout: number
  context?: number[]
}

/** A question to a /lookup route: the answer's outputs, or its error. */
async function query(base: string, q: Record<string, string>): Promise<Out[] | string> {
  const res = await fetch(`${base}/lookup`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ service: 'ls_borg', query: q }) })
  const body = (await res.json()) as { outputs?: Array<{ beef: number[]; outputIndex: number; context?: number[] }>; error?: string }
  if (res.status !== 200) return `${res.status} ${body.error ?? ''}`
  return (body.outputs ?? []).map((o) => ({ tx: Transaction.fromBEEF(o.beef), vout: o.outputIndex, context: o.context }))
}

async function outs(base: string, q: Record<string, string>): Promise<Out[]> {
  const a = await query(base, q)
  if (typeof a === 'string') fail(`${JSON.stringify(q)}: ${a}`)
  return a
}

const ask = async (base: string, q: Record<string, string>): Promise<string[]> => (await outs(base, q)).map((o) => o.tx.id('hex'))

function expect(what: string, got: unknown, want: unknown): void {
  const g = JSON.stringify(got)
  const w = JSON.stringify(want)
  if (g !== w) fail(`${what}: got ${g}, want ${w}`)
  console.log(`ok  ${what}`)
}

function metric(text: string, name: string, ...labels: string[]): string | undefined {
  return text
    .split('\n')
    .find((l) => l.startsWith(name) && labels.every((x) => l.includes(x)))
    ?.trim()
    .split(' ')
    .pop()
}

/**
 * A member reading its group's current epoch key from a host's answers
 * (spec section 6.4): the latest token, the token that began its epoch, its
 * own wrap, the decryption, the commitment. The key's hex, or the label of
 * the check that failed.
 */
async function openEpoch(base: string, org: OrgSim, group: string, member: Party): Promise<string> {
  const latest = await outs(base, { hall, org: org.hex, group })
  if (latest.length !== 1) return `latest: ${latest.length} tokens`
  const head = readToken(latest[0]!.tx).group!
  const began = await outs(base, { hall, org: org.hex, group, seq: String(head.epochSeq) })
  const record = readToken(began[0]!.tx).record
  let refused = 'entry'
  for (const w of await outs(base, { hall, org: org.hex, group, epoch: String(head.epoch), member: member.hex })) {
    const rec = decodeWrap(firstPush(Uint8Array.from(w.tx.outputs[0]!.lockingScript.toBinary()))!)
    let g: Group
    try {
      g = checkEpochToken(record, rec)
    } catch (e) {
      refused = (e as Refusal).reason
      continue
    }
    const i = recipients(rec).findIndex((r) => toHex(r) === member.hex)
    if (i < 0) continue
    try {
      return `epoch ${head.epoch} key ${toHex(await unwrap(member.wallet, rec.entries[i]!, rec.org, rec.epochId, rec.epoch, g.commitment))}`
    } catch (e) {
      refused = (e as Refusal).reason
    }
  }
  return `epoch ${head.epoch} refused ${refused}`
}

async function main(): Promise<void> {
  const dir = process.argv[2]
  if (dir === undefined) fail('usage: node dist/e2e.js /path/to/reference-host')
  const chain = new Chain(30000)
  // The organisation's trees are mined before the host starts, so its header source has a tip.
  const org = new OrgSim(hall, 'e2e organisation', chain)
  const net = new TestNetwork(chain)
  const hdr = await headers(chain, net)
  const db = await mysql()
  const ports: Ports = { db: db.port, headers: (hdr.address() as AddressInfo).port, host: await freePort(), terms: await freePort() }
  const staged = stage(dir)
  let host: Host | undefined
  try {
    host = await startHost(dir, staged, ports)
    const mounted = host.out.join('').split('\n').find((l) => l.includes('borg module mounted'))
    if (mounted === undefined || !mounted.includes('with @lightwebinc/bcommon')) fail(`the bundle's mount line is missing:\n${host.out.join('')}`)
    console.log("ok  the bundle mounts from beside the host's node_modules, reading the host's header source")

    // Create the organisation: three grants, then the token that admits them.
    const [alice, bob, carol] = [new Party('e2e alice'), new Party('e2e bob'), new Party('e2e carol')]
    const ga = await org.grant(alice)
    const gb = await org.grant(bob)
    const gc = await org.grant(carol)
    for (const [name, g] of [['alice', ga], ['bob', gb], ['carol', gc]] as const) expect(`submit the grant to ${name}`, await submit(host, g.tx.toAtomicBEEF(), name), [0])
    const o0 = await org.orgToken([ga.c, gb.c, gc.c])
    // A worse BEEF of the same token first: refused, raising, nothing recorded.
    expect('the organisation token as Atomic BEEF admits nothing', await submit(host, o0.tx.toAtomicBEEF(), 'worse token'), [])
    expect('the organisation token with its parent is admitted after it', await submit(host, o0.beef, 'org 0'), [0])
    expect('org: the latest token', await ask(host.url, { hall, org: org.hex }), [o0.tx.id('hex')])
    const roll = [ga, gb, gc].map((g) => g.tx.id('hex')).sort()
    expect('roll: the three grants', await ask(host.url, { hall, roll: org.hex }), roll)
    const mem = await outs(host.url, { hall, org: org.hex, member: bob.hex })
    const ctx = decodeContext(Uint8Array.from(mem[0]!.context!))
    expect("member: bob's grant, its inclusion path verifying to the roll", [mem.length, verifyGrant(ctx, gb.c, o0.rec.roll)], [1, true])

    // A group of alice and bob, carol added later; wraps of epoch 0.
    const grp = await org.group('engineering', [alice, bob])
    expect('submit the group', await submit(host, grp.tip.beef, 'group 0'), [0])
    for (const w of await grp.wraps()) expect('submit a wrap of epoch 0', await submit(host, w.toAtomicBEEF(), 'wrap'), [0])
    const add = await grp.change({ add: [carol] })
    expect('add carol without a new epoch', [add.rec.epoch, await submit(host, add.beef, 'add')], [0, [0]])
    for (const w of await grp.wraps([carol.id])) expect('submit the wrap of epoch 0 to carol', await submit(host, w.toAtomicBEEF(), 'wrap carol'), [0])
    const k0 = `epoch 0 key ${toHex(grp.epochs.get(0)!.key)}`
    for (const p of [alice, bob, carol]) expect(`${p.label} opens epoch 0 from the host's answers`, await openEpoch(host.url, org, grp.idHex, p), k0)

    // Remove bob: a new epoch whose key he never receives.
    const removal = await grp.change({ remove: [bob] })
    expect('remove bob: a new epoch', [removal.rec.epoch, removal.rec.epochSeq === removal.rec.seq], [1, true])
    expect('submit the removal', await submit(host, removal.beef, 'remove'), [0])
    for (const w of await grp.wraps()) expect('submit a wrap of epoch 1', await submit(host, w.toAtomicBEEF(), 'wrap 1'), [0])
    const k1 = `epoch 1 key ${toHex(grp.epochs.get(1)!.key)}`
    for (const p of [alice, carol]) expect(`${p.label} opens epoch 1`, await openEpoch(host.url, org, grp.idHex, p), k1)
    expect('bob cannot open epoch 1', await openEpoch(host.url, org, grp.idHex, bob), 'epoch 1 refused entry')
    expect('member-of: bob is in no group', await ask(host.url, { hall, org: org.hex, memberOf: bob.hex }), [])

    // Rotate on schedule: epoch 2.
    const rot = await grp.change({ rotate: true })
    expect('submit a rotation', await submit(host, rot.beef, 'rotate'), [0])
    for (const w of await grp.wraps()) await submit(host, w.toAtomicBEEF(), 'wrap 2')
    const k2 = `epoch 2 key ${toHex(grp.epochs.get(2)!.key)}`
    expect('carol opens epoch 2', await openEpoch(host.url, org, grp.idHex, carol), k2)
    expect('bob cannot open epoch 2', await openEpoch(host.url, org, grp.idHex, bob), 'epoch 2 refused entry')

    // Offboard bob: the roll revokes his grant and a sweep spends its outpoint.
    const o1 = await org.orgToken([], [gb.c])
    expect('submit the revocation', await submit(host, o1.beef, 'org 1'), [0])
    const sw = await sweep(org.grantTree, [gb.vout], org.party, KeyGrant, chain)
    expect('submit the sweep', await submit(host, sw.toAtomicBEEF(), 'sweep'), [0])
    expect('roll: alice and carol', await ask(host.url, { hall, roll: org.hex }), [ga, gc].map((g) => g.tx.id('hex')).sort())
    expect('member: bob is no longer in the roll', await ask(host.url, { hall, org: org.hex, member: bob.hex }), [])
    expect("sweep: the spend of bob's grant outpoint", await ask(host.url, { hall, spent: `${org.grantTree.id('hex')}.${gb.vout}` }), [sw.id('hex')])

    const metrics = await (await fetch(`${host.url}/metrics`)).text()
    expect('the refusal is counted by reason', metric(metrics, 'borg_refused_total', 'reason="beef"'), '1')
    expect('the host counts it as one failed topic admission', metric(metrics, 'overlay_host_topic_failures_total', topic), '1')
    expect('every token is consistent', metric(metrics, 'borg_token_status_total', 'status="consistent"'), '6')

    // Priced: refused on the host's own route, 402 then answered on the terms route.
    const history = { hall, history: org.hex, chain: grp.idHex }
    const refused = await query(host.url, history)
    if (typeof refused !== 'string' || !refused.includes('priced')) fail(`history on the host route: ${JSON.stringify(refused)}`)
    console.log('ok  history on the host route is refused: it is priced')
    const terms = (await (await fetch(`${host.terms}/ls_borg/terms`)).json()) as unknown
    expect('the terms document', terms, { service: 'ls_borg', terms: 1, classes: [{ class: 'history', satoshis: 5 }, { class: 'history-after', satoshis: 5 }] })
    expect('a free class on the terms route', await ask(host.terms, { hall, org: org.hex }), [o1.tx.id('hex')])
    expect('history on the terms route without BRC-104', await query(host.terms, history), '401 ')
    const wallet = new PayingWallet(PrivateKey.fromHex('22'.repeat(32)), chain)
    const groupChain = grp.history.map((m) => m.tx.id('hex'))
    const paidHistory = async (): Promise<{ status: number; paid: string | null; txids: string[] }> => {
      const saved = { warn: console.warn, info: console.info }
      console.warn = () => {}
      console.info = () => {}
      try {
        const res = await new AuthFetch(wallet as unknown as WalletInterface).fetch(`${host!.terms}/lookup`, {
          method: 'POST',
          headers: { 'content-type': 'application/json' },
          body: JSON.stringify({ service: 'ls_borg', query: history }),
        })
        const body = (await res.json()) as { outputs?: Array<{ beef: number[] }> }
        return { status: res.status, paid: res.headers.get('x-bsv-payment-satoshis-paid'), txids: (body.outputs ?? []).map((o) => Transaction.fromBEEF(o.beef).id('hex')) }
      } finally {
        Object.assign(console, saved)
      }
    }
    expect("history paid through its 402: the group's whole chain", await paidHistory(), { status: 200, paid: '5', txids: groupChain })
    const lines = readFileSync(join(staged.root, 'state', 'payments.jsonl'), 'utf8').trim().split('\n').map((l) => JSON.parse(l) as { txid: string; decision?: string })
    expect('the payment is in the ledger, taken fast', lines.map((l) => l.decision), ['fast'])
    expect('the host broadcast it before answering', lines.every((l) => net.sent.includes(l.txid)), true)

    // Restart: the index is rebuilt from storage and the rows.
    const before = {
      org: await ask(host.url, { hall, org: org.hex }),
      roll: await ask(host.url, { hall, roll: org.hex }),
      groups: await ask(host.url, { hall, groupsOf: org.hex }),
      memberOfAlice: await ask(host.url, { hall, org: org.hex, memberOf: alice.hex }),
      wraps2: await ask(host.url, { hall, org: org.hex, group: grp.idHex, epoch: '2' }),
      seq0: await ask(host.url, { hall, org: org.hex, group: grp.idHex, seq: '0' }),
    }
    await stopHost(host)
    host = await startHost(dir, staged, ports)
    const restored = host.out.join('').split('\n').find((l) => l.includes('module lookup restored from storage'))
    if (restored === undefined) fail(`no restore line:\n${host.out.join('')}`)
    console.log('ok  the module restored from storage')
    const after = {
      org: await ask(host.url, { hall, org: org.hex }),
      roll: await ask(host.url, { hall, roll: org.hex }),
      groups: await ask(host.url, { hall, groupsOf: org.hex }),
      memberOfAlice: await ask(host.url, { hall, org: org.hex, memberOf: alice.hex }),
      wraps2: await ask(host.url, { hall, org: org.hex, group: grp.idHex, epoch: '2' }),
      seq0: await ask(host.url, { hall, org: org.hex, group: grp.idHex, seq: '0' }),
    }
    expect('restored: the same answers, spent tokens included', after, before)
    expect('restored: carol opens epoch 2', await openEpoch(host.url, org, grp.idHex, carol), k2)
    expect('restored: bob still cannot', await openEpoch(host.url, org, grp.idHex, bob), 'epoch 2 refused entry')
    expect("restored: bob's grant stays swept", await ask(host.url, { hall, spent: `${org.grantTree.id('hex')}.${gb.vout}` }), [sw.id('hex')])
    expect('restored: history paid again', await paidHistory(), { status: 200, paid: '5', txids: groupChain })
    expect('restored: a replayed token is a duplicate', await submit(host, removal.beef, 'removal again'), [])
    const m2 = await (await fetch(`${host.url}/metrics`)).text()
    expect('restored: every token folded consistent again', metric(m2, 'borg_token_status_total', 'status="consistent"'), '6')
    console.log('e2e: PASS')
  } finally {
    if (host !== undefined) await stopHost(host)
    docker('rm', '-f', db.id)
    hdr.close()
    rmSync(staged.root, { recursive: true, force: true })
  }
}

main().catch((e: unknown) => {
  console.error(e instanceof Error ? e.message : e)
  process.exit(1)
})
