/**
 * The shipped file rather than the tsc tree: bundle/borg-module.js as the
 * bundle step leaves it, mounted the way the host mounts a module, admitting
 * golden carriers, tokens and a sweep against a header source over HTTP,
 * and answering ls_borg. Every other test runs on dist/, which resolves the
 * library from node_modules, so only this one proves the bundle works with
 * the library inlined and nothing but @bsv/sdk and Node's built-ins beside
 * it.
 */
import { test, after } from 'node:test'
import assert from 'node:assert/strict'
import { existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { createServer, type Server } from 'node:http'
import type { AddressInfo } from 'node:net'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import type { Module, ModuleHost } from '@lightwebinc/bcommon'
import { FakeHost, beefOf, byName, heldOf, txOf, txVectors } from './testutil.js'

const v = txVectors()
// dist/bundle.test.js -> host/. Imported by URL, so the type checker does not
// go looking for a file that exists only after the bundle step.
const bundleUrl = new URL('../bundle/borg-module.js', import.meta.url)

function version(pkg: string): string {
  const url = new URL(`../node_modules/${pkg}/package.json`, import.meta.url)
  return (JSON.parse(readFileSync(url, 'utf8')) as { version: string }).version
}

/** The vectors' headers in the native shape a module reads. */
async function headerSource(): Promise<string> {
  const roots = new Map(v.headers.map((h) => [h.height, h.merkleRoot]))
  const server: Server = createServer((req, res) => {
    const m = /^\/v1\/root\/(\d+)$/.exec(req.url ?? '')
    const body = req.url === '/v1/tip' ? { height: Math.max(...roots.keys()) } : m !== null && roots.has(Number(m[1])) ? { merkleRoot: roots.get(Number(m[1])) } : undefined
    res.writeHead(body === undefined ? 404 : 200, { 'content-type': 'application/json' })
    res.end(JSON.stringify(body ?? {}))
  })
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r))
  after(() => server.close())
  return `http://127.0.0.1:${(server.address() as AddressInfo).port}`
}

async function mount(host: FakeHost, dir: string, headers: string): Promise<Module> {
  const { default: create } = (await import(bundleUrl.href)) as { default: (host: ModuleHost) => Promise<Module> }
  const keys = ['BORG_HALLS', 'BORG_STATE_DIR', 'BORG_HEADERS_URL', 'OVERLAY_TOPICS'] as const
  const saved = Object.fromEntries(keys.map((k) => [k, process.env[k]]))
  try {
    process.env['BORG_HALLS'] = v.hall
    process.env['BORG_STATE_DIR'] = dir
    process.env['BORG_HEADERS_URL'] = headers
    process.env['OVERLAY_TOPICS'] = `tm_anytx,${v.topic}`
    return await create(host)
  } finally {
    for (const k of keys) {
      if (saved[k] === undefined) delete process.env[k]
      else process.env[k] = saved[k]
    }
  }
}

test('the shipped bundle admits golden carriers, tokens and a sweep, refuses by raising, and ls_borg answers them', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'borg-bundle-'))
  try {
    const host = new FakeHost()
    const mod = await mount(host, dir, await headerSource())
    const tm = mod.topics?.[v.topic]
    const ls = mod.lookups?.['ls_borg']
    if (tm === undefined || ls === undefined) throw new Error('the bundle did not mount the hall and ls_borg')
    for (const name of ['grant-carrier-alice', 'grant-carrier-bob', 'grant-carrier-carol', 'org-token-genesis', 'org-token-revoke', 'sweep']) {
      const t = byName(v, name)
      assert.deepEqual(await tm.identifyAdmissibleOutputs(beefOf(t), heldOf(t)), { outputsToAdmit: t.admits!.outputsToAdmit, coinsToRetain: t.admits!.coinsToRetain }, name)
      for (const i of t.admits!.outputsToAdmit) await ls.outputAdmittedByTopic({ mode: 'whole-tx', atomicBEEF: txOf(t).toAtomicBEEF(), outputIndex: i, topic: v.topic })
    }
    assert.equal(host.count('borg_admitted_total', { kind: 'grant' }), 3)
    assert.equal(host.count('borg_admitted_total', { kind: 'org-token' }), 2)
    // A refusal raises, with the label the tsc tree counts under.
    await assert.rejects(tm.identifyAdmissibleOutputs(beefOf(byName(v, 'carrier-final-input')), []), /mineable/)
    assert.equal(host.count('borg_refused_total', { reason: 'mineable' }), 1)
    // A token whose parent's block the header source does not hold: unmined.
    await assert.rejects(tm.identifyAdmissibleOutputs(beefOf(byName(v, 'token-parent-proof-unknown-height')), [0]), /unmined/)
    const org = '03baf7689c0a3558fb604589036a8d1e4b685d909f6e0e2c6018a14049ae64ec26'
    const roll = await ls.lookup({ service: 'ls_borg', query: { hall: v.hall, roll: org } })
    assert.deepEqual(roll.map((f) => f.txid).sort(), [byName(v, 'grant-carrier-alice').txid, byName(v, 'grant-carrier-carol').txid].sort())
    const latest = await ls.lookup({ service: 'ls_borg', query: { hall: v.hall, org } })
    assert.deepEqual(latest.map((f) => f.txid), [byName(v, 'org-token-revoke').txid])
    assert.equal(host.gauges.get('borg_tokens')?.(), 2)
    assert.ok(existsSync(join(dir, 'borg-rows.jsonl')), 'the rows are on disk')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('the bundle names its versions in its banner and in the host log', async () => {
  const banner = readFileSync(bundleUrl, 'utf8').split('\n', 1)[0] ?? ''
  assert.ok(banner.startsWith('// @lightwebinc/borg-host '), banner)
  assert.ok(banner.includes(`@lightwebinc/bcommon ${version('@lightwebinc/bcommon')} inlined`), banner)
  assert.ok(banner.includes(`imports @bsv/sdk (built against ${version('@bsv/sdk')})`), banner)
  const dir = mkdtempSync(join(tmpdir(), 'borg-bundle-'))
  try {
    const host = new FakeHost()
    await mount(host, dir, 'http://127.0.0.1:1')
    const mounted = host.lines.filter((l) => l.msg === 'borg module mounted')
    assert.equal(mounted.length, 1)
    assert.equal(`// ${String(mounted[0]?.extra?.['build'])}`, banner)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('the bundle imports nothing but @bsv/sdk and Node built-ins, and carries no source map', () => {
  const text = readFileSync(bundleUrl, 'utf8')
  const specifiers = new Set([...text.matchAll(/^\s*(?:import|export)\b[^'"]*?from\s*["']([^"']+)["']/gm)].map((m) => m[1]!))
  assert.ok(specifiers.has('@bsv/sdk'))
  assert.deepEqual([...specifiers].filter((s) => s !== '@bsv/sdk' && !s.startsWith('node:')), [])
  assert.equal(/\bimport\s*\(|\brequire\s*\(/.test(text), false, 'no dynamic import or require')
  assert.equal(text.includes('sourceMappingURL'), false)
  assert.equal(existsSync(new URL('../bundle/borg-module.js.map', import.meta.url)), false)
})
