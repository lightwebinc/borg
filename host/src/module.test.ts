/**
 * The compiled module as a reference host loads it: imported by absolute
 * path, its default export called with the host's logger and metrics, the
 * result held to the loader's shape rules, and its configuration refused
 * rather than guessed.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import type { Module, ModuleHost } from '@lightwebinc/bcommon'
import { parseConfig, parseHalls } from './module.js'
import { FakeHost } from './testutil.js'

const hall = 'example_hall_vkqzmtrwxp'
const other = 'support_mkvbqzxtrw'
const headers = { BORG_HEADERS_URL: 'http://127.0.0.1:1' }

test('BORG_HALLS: hall identifiers, once each; anything else refused', () => {
  assert.deepEqual(parseHalls(` ${hall},${other} `), [hall, other])
  for (const bad of [undefined, '', 'example_hall', `${hall},${hall}`, 'Example_hall_vkqzmtrwxp', `${hall},`, 'a__b_abcdefghij']) {
    assert.throws(() => parseHalls(bad), JSON.stringify(bad))
  }
})

test('the configuration: a state directory and a native header source are required, floors hold, and a price needs its route and payee', () => {
  const base = { BORG_HALLS: hall, BORG_STATE_DIR: '/var/lib/borg', ...headers }
  const c = parseConfig(base)
  assert.deepEqual([c.retentionDays, c.maxBEEF, c.maxSweepInputs, c.spendTable, c.prices.size, c.listen, c.headersURL], [31, 262144, 1024, 65536, 0, undefined, 'http://127.0.0.1:1'])
  assert.throws(() => parseConfig({ BORG_HALLS: hall, ...headers }), /BORG_STATE_DIR/)
  assert.throws(() => parseConfig({ ...base, BORG_STATE_DIR: 'relative' }), /BORG_STATE_DIR/)
  // The header source: the module's own, or the host's when it is a native URL.
  assert.equal(parseConfig({ BORG_HALLS: hall, BORG_STATE_DIR: '/x', OVERLAY_CHAIN_TRACKER_URL: 'https://headers.example' }).headersURL, 'https://headers.example')
  assert.throws(() => parseConfig({ BORG_HALLS: hall, BORG_STATE_DIR: '/x', OVERLAY_CHAIN_TRACKER_URL: 'woc:main' }), /BORG_HEADERS_URL is required/)
  assert.throws(() => parseConfig({ BORG_HALLS: hall, BORG_STATE_DIR: '/x' }), /BORG_HEADERS_URL is required/)
  assert.throws(() => parseConfig({ ...base, BORG_HEADERS_URL: 'ftp://x' }), /BORG_HEADERS_URL/)
  assert.throws(() => parseConfig({ ...base, BORG_RETENTION_DAYS: '30' }), /at least 31/)
  assert.throws(() => parseConfig({ ...base, BORG_MAX_BEEF: '1000' }), /at least 262144/)
  assert.throws(() => parseConfig({ ...base, BORG_MAX_SWEEP_INPUTS: '1023' }), /at least 1024/)
  assert.throws(() => parseConfig({ ...base, BORG_SPEND_TABLE: '100' }), /at least 65536/)
  assert.throws(() => parseConfig({ ...base, BORG_LISTEN: 'nowhere' }), /host:port/)
  assert.throws(() => parseConfig({ ...base, BORG_PAYEE_KEY: 'AB'.repeat(32) }), /64 lowercase hex/)
  assert.throws(() => parseConfig({ ...base, BORG_PRICES: 'history=5,history-after=5' }), /BORG_LISTEN/)
  assert.throws(() => parseConfig({ ...base, BORG_PRICES: 'history=5', BORG_LISTEN: '127.0.0.1:1', BORG_PAYEE_KEY: '11'.repeat(32) }), /history and history-after are priced together/)
  assert.throws(() => parseConfig({ ...base, BORG_PRICES: 'history=0' }), /BORG_LISTEN/)
  assert.throws(() => parseConfig({ ...base, BORG_PRICES: 'org=1', BORG_LISTEN: '127.0.0.1:1' }), /free class/)
  const priced = parseConfig({ ...base, BORG_PRICES: 'history=5,history-after=5', BORG_LISTEN: '[::1]:8081', BORG_PAYEE_KEY: '11'.repeat(32) })
  assert.deepEqual([priced.listen, priced.prices.get('history')], [{ host: '::1', port: 8081 }, 5])
  assert.equal(parseConfig({ ...base, BORG_RETENTION_DAYS: '90', BORG_MAX_BEEF: '1048576', BORG_SPEND_TABLE: '100000' }).spendTable, 100000)
  assert.deepEqual(c.responses, { perSec: 5, burst: 20 }, 'the budget of signed responses a session has a default')
  assert.deepEqual(parseConfig({ ...base, BORG_RESPONSES_PER_SEC: '0.5', BORG_RESPONSE_BURST: '3' }).responses, { perSec: 0.5, burst: 3 })
  assert.throws(() => parseConfig({ ...base, BORG_RESPONSE_BURST: '0' }), /BORG_RESPONSE_BURST/)
  assert.throws(() => parseConfig({ ...base, BORG_RESPONSES_PER_SEC: '0' }), /BORG_RESPONSES_PER_SEC/)
  assert.deepEqual(c.sessions, { max: 10000, ttlSeconds: 600 }, 'the BRC-104 session bound has a default')
  assert.deepEqual(parseConfig({ ...base, BORG_SESSIONS: '50', BORG_SESSION_TTL: '60' }).sessions, { max: 50, ttlSeconds: 60 })
  assert.throws(() => parseConfig({ ...base, BORG_SESSIONS: '0' }), /BORG_SESSIONS/)
  assert.deepEqual(c.handshakes, { perSec: 4, burst: 8, perAddressPerSec: 1, addressBurst: 4 }, 'the handshake budget has a default')
  assert.deepEqual(
    parseConfig({ ...base, BORG_HANDSHAKES_PER_SEC: '2.5', BORG_HANDSHAKE_BURST: '5', BORG_HANDSHAKES_PER_ADDR_PER_SEC: '0.5', BORG_HANDSHAKE_ADDR_BURST: '2' }).handshakes,
    { perSec: 2.5, burst: 5, perAddressPerSec: 0.5, addressBurst: 2 },
  )
  for (const [k, v] of [['BORG_HANDSHAKES_PER_SEC', '0'], ['BORG_HANDSHAKES_PER_ADDR_PER_SEC', 'fast'], ['BORG_HANDSHAKE_BURST', '0'], ['BORG_HANDSHAKE_ADDR_BURST', '1.5']]) {
    assert.throws(() => parseConfig({ ...base, [k!]: v }), new RegExp(k!))
  }
})

/** The reference loader's checks on what a factory returned. */
function loaderShape(m: unknown): Module {
  assert.ok(typeof m === 'object' && m !== null)
  const mod = m as Module
  assert.ok(Object.keys(mod.topics ?? {}).length + Object.keys(mod.lookups ?? {}).length > 0, 'mounts something')
  for (const tm of Object.values(mod.topics ?? {})) assert.equal(typeof tm.identifyAdmissibleOutputs, 'function')
  for (const ls of Object.values(mod.lookups ?? {})) {
    assert.equal(typeof ls.lookup, 'function')
    assert.equal(typeof ls.outputAdmittedByTopic, 'function')
    assert.equal(typeof ls.admissionMode, 'string')
    assert.equal(typeof (ls as { restore?: unknown }).restore, 'function', 'the index is in memory, so the module must restore')
  }
  return mod
}

test('dist/module.js loads by absolute path, as OVERLAY_MODULES names it, and mounts each hall and ls_borg', async () => {
  const path = fileURLToPath(new URL('./module.js', import.meta.url))
  const imported = (await import(pathToFileURL(path).href)) as { default: (h: ModuleHost) => Promise<Module> }
  assert.equal(typeof imported.default, 'function')
  const keys = ['BORG_HALLS', 'BORG_STATE_DIR', 'BORG_HEADERS_URL', 'OVERLAY_TOPICS'] as const
  const saved = Object.fromEntries(keys.map((k) => [k, process.env[k]]))
  const dir = mkdtempSync(join(tmpdir(), 'borg-module-'))
  try {
    process.env['BORG_HALLS'] = `${hall},${other}`
    process.env['BORG_STATE_DIR'] = dir
    process.env['BORG_HEADERS_URL'] = 'http://127.0.0.1:1'
    process.env['OVERLAY_TOPICS'] = `tm_anytx,tm_borg_${hall},tm_borg_${other}`
    const host = new FakeHost()
    const mod = loaderShape(await imported.default(host))
    assert.deepEqual(Object.keys(mod.topics!).sort(), [`tm_borg_${hall}`, `tm_borg_${other}`])
    assert.deepEqual(Object.keys(mod.lookups!), ['ls_borg'])
    assert.ok(host.counters.has('borg_refused_total{reason="beef"}'), 'counters are preset')
    assert.ok(host.counters.has('borg_token_status_total{status="rejoin-unrotated"}'))
    assert.ok(host.gauges.has('borg_outpoint_rows'))
    assert.ok(host.gauges.has('borg_spend_table'))
    // A tm_borg_ topic the host carries but the module does not configure
    // would fall to the admit-everything default: refused.
    process.env['OVERLAY_TOPICS'] = `tm_borg_${hall},tm_borg_third_mkvbqzxtrw`
    await assert.rejects(imported.default(new FakeHost()), /tm_borg_third_mkvbqzxtrw/)
    delete process.env['BORG_HALLS']
    await assert.rejects(imported.default(new FakeHost()), /BORG_HALLS/)
  } finally {
    for (const k of keys) {
      if (saved[k] === undefined) delete process.env[k]
      else process.env[k] = saved[k]
    }
    rmSync(dir, { recursive: true, force: true })
  }
})
