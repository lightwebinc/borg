/**
 * The borg host module: `tm_borg_<name>_<suffix>` for each configured hall
 * and `ls_borg` for all of them, loaded into a reference overlay host by
 * path (`OVERLAY_MODULES=/abs/path/to/borg-module.js`). The host carries no
 * borg code; this file's default export is the factory it calls.
 *
 * Configuration is read from the environment when the host calls the
 * factory (docs/host.md):
 *
 *   BORG_HALLS            required: the hall identifiers, a comma list
 *   BORG_STATE_DIR        required: an absolute directory kept for the host's
 *                         life, for the outpoint rows and the payment ledger
 *   BORG_HEADERS_URL      the native header source (/v1/root/<height>,
 *                         /v1/tip) admission checks proofs against;
 *                         OVERLAY_CHAIN_TRACKER_URL when that is one
 *   BORG_RETENTION_DAYS   how long what is no longer answered is kept; at
 *                         least and by default 31
 *   BORG_MAX_BEEF         the host's BEEF bound; at least and by default 262144
 *   BORG_MAX_SWEEP_INPUTS the inputs of a sweep the host accepts; at least
 *                         and by default 1024
 *   BORG_SPEND_TABLE      spends of outpoints no carrier used that the host
 *                         keeps; at least and by default 65536
 *   BORG_LISTEN           host:port of the terms route (paid.ts)
 *   BORG_PRICES           the classes priced, e.g. history=5,history-after=5
 *                         (a class and its -after form together)
 *   BORG_PAYEE_KEY        the payee's private key, 64 hex characters
 *   BORG_SESSIONS         the most BRC-104 sessions the terms route keeps;
 *                         default 10000
 *   BORG_SESSION_TTL      seconds an idle BRC-104 session is kept; default 600
 *   BORG_HANDSHAKES_PER_SEC, BORG_HANDSHAKE_BURST
 *                         the BRC-104 handshakes a second the terms route
 *                         answers, and at once; default 4 and 8 (bcommon host)
 *   BORG_HANDSHAKES_PER_ADDR_PER_SEC, BORG_HANDSHAKE_ADDR_BURST
 *                         the same for one remote address; default 1 and 4
 *   BORG_ARCADE_URL       arcade, which the host broadcasts payments through
 *   BORG_ASSET_URL        the node's asset service: spend view and proofs;
 *                         without both, every payment is held until mined
 *   BORG_ACCEPT_THRESHOLD_SATS
 *                         the largest payment answered on network acceptance;
 *                         default 25000000; above it, held until mined
 *   BORG_ACCEPT_PAYER_LIMIT, BORG_ACCEPT_TOTAL_LIMIT
 *                         satoshis taken fast and not yet mined, per payer
 *                         and in all; default one and ten thresholds
 *   BORG_ACCEPT_WINDOW    how long a fast payment counts against the limits
 *                         unless it mines first; default 1h
 *   BORG_ACCEPT_WATCH     how long to watch for a conflict before answering;
 *                         default 0s
 *   BORG_RESPONSES_PER_SEC, BORG_RESPONSE_BURST
 *                         the signed responses a second one BRC-104 session
 *                         is given, and at once; default 5 and 20
 *
 * Every `tm_borg_` topic in OVERLAY_TOPICS must be a hall named here,
 * because a borg topic left to the host's default manager would admit
 * anything. Every mistake throws, and the host refuses to start.
 */
import { isAbsolute } from 'node:path'
import type { Server } from 'node:http'
import { PrivateKey, ProtoWallet, type ChainTracker } from '@bsv/sdk'
import { Transaction, Utils } from '@bsv/sdk'
import type { Module, ModuleHost } from '@lightwebinc/bcommon'
import {
  DefaultBudget,
  DefaultMaxSessions,
  DefaultResponseBudget,
  DefaultSessionTTL,
  gateFor,
  LedgerReceiver,
  LookupFront,
  parseAcceptConfig,
  type AcceptConfig,
  type BudgetConfig,
  type ResponseBudgetConfig,
} from '@lightwebinc/bcommon/host'
import { DefaultMaxBEEF, DefaultMaxSweepInputs } from './beef.js'
import { LookupService, TopicPrefix, checkHall, topic } from './borgrec.js'
import { HeaderTracker, isNativeSource } from './headers.js'
import { FileJournal, type Journal } from './journal.js'
import { BorgLookupService, FloorDays, MinSpendTable } from './ls_borg.js'
import { Ledger, borgRoute, parsePrices } from './paid.js'
import { AdmitKinds, BorgTopicManager, Reasons } from './tm_borg.js'

/**
 * The bundle's banner, which the bundle step defines (scripts/bundle.js):
 * the package and library versions the host is running. The tsc tree
 * leaves it undefined.
 */
declare const BORG_BUILD: string | undefined

/** How often retention runs, seconds. */
const PruneEvery = 3600
/** How often the background tick runs (payment watch; retention every PruneEvery), seconds. */
const TickEvery = 30

/** The token statuses the index reports (spec section 11.2), each counted. */
export const TokenStatuses = ['consistent', 'pending', 'roll-mismatch', 'members-mismatch', 'commitment-reuse', 'rejoin-unrotated'] as const

export interface Config {
  halls: string[]
  stateDir: string
  headersURL: string
  retentionDays: number
  maxBEEF: number
  maxSweepInputs: number
  spendTable: number
  listen?: { host: string; port: number }
  prices: Map<string, number>
  payeeKey?: PrivateKey
  sessions: { max: number; ttlSeconds: number }
  handshakes: BudgetConfig
  responses: ResponseBudgetConfig
  /** Payment acceptance; bcommon's DefaultPolicy with no arcade or node (every payment held) when unset. */
  accept?: AcceptConfig
}

/** Parses BORG_HALLS. Throws on anything it cannot take exactly. */
export function parseHalls(raw: string | undefined): string[] {
  if (raw === undefined || raw.trim() === '') throw new Error('BORG_HALLS is required: name each hall this host carries')
  const out: string[] = []
  for (const entry of raw.split(',')) {
    const hall = entry.trim()
    try {
      checkHall(hall)
    } catch {
      throw new Error(`BORG_HALLS: "${hall}" is not a hall identifier <name>_<suffix>`)
    }
    if (out.includes(hall)) throw new Error(`BORG_HALLS names ${hall} twice`)
    out.push(hall)
  }
  return out
}

function integer(name: string, raw: string | undefined, def: number, min: number): number {
  if (raw === undefined || raw.trim() === '') return def
  const v = raw.trim()
  if (!/^[0-9]+$/.test(v) || !Number.isSafeInteger(Number(v)) || Number(v) < min) throw new Error(`${name}: "${raw}" is not an integer of at least ${min}`)
  return Number(v)
}

/** A positive rate, decimals allowed, at most 1e6. */
function rate(name: string, raw: string | undefined, def: number): number {
  if (raw === undefined || raw.trim() === '') return def
  const v = raw.trim()
  if (!/^[0-9]+(\.[0-9]+)?$/.test(v) || !(Number(v) > 0) || Number(v) > 1e6) throw new Error(`${name}: "${raw}" is not a positive number of at most 1000000`)
  return Number(v)
}

/** Reads the whole configuration from env. */
export function parseConfig(env: NodeJS.ProcessEnv): Config {
  const halls = parseHalls(env['BORG_HALLS'])
  const stateDir = env['BORG_STATE_DIR']
  if (stateDir === undefined || !isAbsolute(stateDir)) {
    throw new Error("BORG_STATE_DIR is required: an absolute directory kept for the host's life, where the outpoint rows live")
  }
  const own = env['BORG_HEADERS_URL']?.trim()
  const host = env['OVERLAY_CHAIN_TRACKER_URL']?.trim()
  let headersURL: string
  if (own !== undefined && own !== '') {
    if (!isNativeSource(own)) throw new Error(`BORG_HEADERS_URL: "${own}" is not an http or https URL`)
    headersURL = own
  } else if (host !== undefined && isNativeSource(host)) {
    headersURL = host
  } else {
    throw new Error('BORG_HEADERS_URL is required: a native header source (/v1/root/<height>, /v1/tip) that token and sweep proofs are checked against; OVERLAY_CHAIN_TRACKER_URL is not one')
  }
  const c: Config = {
    halls,
    stateDir,
    headersURL,
    retentionDays: integer('BORG_RETENTION_DAYS', env['BORG_RETENTION_DAYS'], FloorDays, FloorDays),
    maxBEEF: integer('BORG_MAX_BEEF', env['BORG_MAX_BEEF'], DefaultMaxBEEF, DefaultMaxBEEF),
    maxSweepInputs: integer('BORG_MAX_SWEEP_INPUTS', env['BORG_MAX_SWEEP_INPUTS'], DefaultMaxSweepInputs, DefaultMaxSweepInputs),
    spendTable: integer('BORG_SPEND_TABLE', env['BORG_SPEND_TABLE'], MinSpendTable, MinSpendTable),
    prices: parsePrices(env['BORG_PRICES']),
    sessions: {
      max: integer('BORG_SESSIONS', env['BORG_SESSIONS'], DefaultMaxSessions, 1),
      ttlSeconds: integer('BORG_SESSION_TTL', env['BORG_SESSION_TTL'], DefaultSessionTTL, 1),
    },
    handshakes: {
      perSec: rate('BORG_HANDSHAKES_PER_SEC', env['BORG_HANDSHAKES_PER_SEC'], DefaultBudget.perSec),
      burst: integer('BORG_HANDSHAKE_BURST', env['BORG_HANDSHAKE_BURST'], DefaultBudget.burst, 1),
      perAddressPerSec: rate('BORG_HANDSHAKES_PER_ADDR_PER_SEC', env['BORG_HANDSHAKES_PER_ADDR_PER_SEC'], DefaultBudget.perAddressPerSec),
      addressBurst: integer('BORG_HANDSHAKE_ADDR_BURST', env['BORG_HANDSHAKE_ADDR_BURST'], DefaultBudget.addressBurst, 1),
    },
    responses: {
      perSec: rate('BORG_RESPONSES_PER_SEC', env['BORG_RESPONSES_PER_SEC'], DefaultResponseBudget.perSec),
      burst: integer('BORG_RESPONSE_BURST', env['BORG_RESPONSE_BURST'], DefaultResponseBudget.burst, 1),
    },
    accept: parseAcceptConfig('BORG', env),
  }
  const listen = env['BORG_LISTEN']?.trim()
  if (listen !== undefined && listen !== '') {
    const m = /^(.+):([0-9]{1,5})$/.exec(listen)
    const port = m === null ? NaN : Number(m[2])
    if (m === null || port < 1 || port > 65535) throw new Error(`BORG_LISTEN: "${listen}" is not host:port`)
    c.listen = { host: m[1]!.replace(/^\[(.*)\]$/, '$1'), port }
  }
  const key = env['BORG_PAYEE_KEY']?.trim()
  if (key !== undefined && key !== '') {
    if (!/^[0-9a-f]{64}$/.test(key)) throw new Error('BORG_PAYEE_KEY is not 64 lowercase hex characters')
    c.payeeKey = PrivateKey.fromHex(key)
  }
  const charged = [...c.prices.values()].some((p) => p > 0)
  if (charged && (c.listen === undefined || c.payeeKey === undefined)) {
    throw new Error('BORG_PRICES prices a class: BORG_LISTEN and BORG_PAYEE_KEY are required')
  }
  if (c.prices.size > 0 && c.listen === undefined) throw new Error('BORG_PRICES needs BORG_LISTEN, where the terms document is served')
  return c
}

export interface Mounted {
  module: Module
  ls: BorgLookupService
  /** The terms route, when configured. */
  front?: LookupFront
  /** Starts the terms route (when configured) and retention; resolves when the route listens. */
  start: () => Promise<Server | undefined>
}

/**
 * The module for this configuration. `overlayTopics` is OVERLAY_TOPICS,
 * when known; `journal` replaces the state directory's, `headers` the
 * configured header source and `now` the host's clock, for tests.
 */
export function borgModule(host: ModuleHost, c: Config, overlayTopics?: readonly string[], journal?: Journal, headers?: ChainTracker, now?: () => number): Mounted {
  const names = new Set(c.halls.map(topic))
  for (const t of overlayTopics ?? []) {
    if (t.startsWith(TopicPrefix) && !names.has(t)) {
      throw new Error(`OVERLAY_TOPICS names ${t} but BORG_HALLS does not configure it; it would be admitted by the default manager`)
    }
  }
  for (const kind of AdmitKinds) host.metrics.preset('borg_admitted_total', { kind })
  for (const kind of AdmitKinds) host.metrics.preset('borg_admitted_repeats_total', { kind })
  for (const reason of Reasons) host.metrics.preset('borg_refused_total', { reason })
  for (const status of TokenStatuses) host.metrics.preset('borg_token_status_total', { status })
  for (const why of ['evidence', 'retention', 'table']) host.metrics.preset('borg_dropped_total', { why })
  for (const why of ['cap', 'idle']) host.metrics.preset('borg_sessions_evicted_total', { why })
  host.metrics.preset('borg_retractions_total')
  const priced = new Set([...c.prices].filter(([, p]) => p > 0).map(([name]) => name))
  const ls = new BorgLookupService({
    halls: new Set(c.halls),
    host,
    journal: journal ?? new FileJournal(c.stateDir),
    priced,
    retentionDays: c.retentionDays,
    spendTable: c.spendTable,
    now,
  })
  host.metrics.gauge('borg_halls', () => c.halls.length)
  host.metrics.gauge('borg_tokens', () => ls.tokenCount)
  host.metrics.gauge('borg_tokens_pending', () => ls.pendingTokens)
  host.metrics.gauge('borg_grants', () => ls.grantCount)
  host.metrics.gauge('borg_wraps', () => ls.wrapCount)
  host.metrics.gauge('borg_sweeps', () => ls.sweepCount)
  host.metrics.gauge('borg_outpoint_rows', () => ls.rowCount)
  host.metrics.gauge('borg_carrier_lines', () => ls.lineCount)
  host.metrics.gauge('borg_spend_table', () => ls.tableSize)
  const tracker = headers ?? new HeaderTracker(c.headersURL)
  const topics: Record<string, BorgTopicManager> = {}
  for (const hall of c.halls) topics[topic(hall)] = new BorgTopicManager(topic(hall), host, tracker, c.maxBEEF, c.maxSweepInputs)
  let front: LookupFront | undefined
  const accept = c.accept ?? parseAcceptConfig('BORG', {})
  const receiver = c.listen !== undefined && priced.size > 0 ? new LedgerReceiver(c.stateDir, Ledger) : undefined
  const gate = receiver !== undefined ? gateFor('borg', host, tracker, accept, c.stateDir) : undefined
  if (gate !== undefined && receiver !== undefined) {
    for (const f of receiver.fast) {
      try {
        gate.restore(Transaction.fromAtomicBEEF(Utils.toArray(f.beef, 'base64')), f.senderIdentityKey, f.satoshis, f.at)
      } catch {
        // a line whose BEEF does not parse was never answered fast
      }
    }
  }
  if (c.listen !== undefined) {
    front = new LookupFront({
      ...borgRoute(ls),
      host,
      prices: c.prices,
      wallet: c.payeeKey === undefined ? undefined : new ProtoWallet(c.payeeKey),
      receiver,
      headers: tracker,
      gate,
      sessions: c.sessions,
      budget: c.handshakes,
      responses: c.responses,
    })
  }
  const start = async (): Promise<Server | undefined> => {
    let pruned = Date.now()
    let sweeping = false
    const timer = setInterval(() => {
      if (gate !== undefined && !sweeping) {
        sweeping = true
        gate
          .sweep()
          .catch((e: unknown) => host.log('borg payment watch failed', { err: String(e) }))
          .finally(() => (sweeping = false))
      }
      if (!ls.restored || Date.now() - pruned < PruneEvery * 1000) return
      pruned = Date.now()
      ls.prune().catch((e: unknown) => host.log('ls_borg retention failed', { err: String(e) }))
    }, TickEvery * 1000)
    timer.unref()
    if (front === undefined || c.listen === undefined) return undefined
    const server = await front.listen(c.listen.port, c.listen.host)
    server.unref()
    return server
  }
  const build = typeof BORG_BUILD === 'string' ? BORG_BUILD : 'unbundled'
  host.log('borg module mounted', {
    topics: [...names].join(','),
    lookup: LookupService,
    headers: headers === undefined ? c.headersURL : 'supplied',
    terms: c.listen === undefined ? 'none' : `${c.listen.host}:${c.listen.port}`,
    priced: [...priced].join(',') || 'none',
    sessions: c.listen === undefined ? 'none' : `${c.sessions.max}, idle ${c.sessions.ttlSeconds}s`,
    handshakes: c.listen === undefined ? 'none' : `${c.handshakes.perSec}/s burst ${c.handshakes.burst}, per address ${c.handshakes.perAddressPerSec}/s burst ${c.handshakes.addressBurst}`,
    responses: c.listen === undefined ? 'none' : `${c.responses.perSec}/s burst ${c.responses.burst} a session`,
    payments: gate === undefined ? 'none' : gate.networked ? `fast to ${accept.policy.thresholdSats} sat, payer ${accept.policy.payerLimit}, total ${accept.policy.totalLimit}` : 'held until mined (no arcade or asset URL)',
    build,
  })
  return { module: { topics, lookups: { [LookupService]: ls } }, ls, front, start }
}

/** The factory the host calls: the configuration from the environment, checked against OVERLAY_TOPICS. */
export default async function create(host: ModuleHost): Promise<Module> {
  const overlay = process.env['OVERLAY_TOPICS']
  const topics = overlay === undefined ? undefined : overlay.split(',').map((t) => t.trim()).filter((t) => t !== '')
  const m = borgModule(host, parseConfig(process.env), topics)
  await m.start()
  return m.module
}

export { BorgTopicManager, Refused } from './tm_borg.js'
export { BorgLookupService } from './ls_borg.js'
export { LookupFront } from '@lightwebinc/bcommon/host'
export { termsDocument } from './paid.js'
