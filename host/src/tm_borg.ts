/**
 * `tm_borg_<name>_<suffix>`: the topic manager of one hall (spec section
 * 11.1), one instance per configured hall. The rules are admit.ts's; this
 * file is the engine seam: it runs them on the submission and turns the
 * verdict into admittance instructions.
 *
 * The overlay engine records every transaction a topic manager returns
 * instructions for as applied, empty instructions included: a later
 * submission of the same txid is a duplicate no topic manager is shown
 * again. A refusal returned as empty instructions would therefore be
 * permanent for that txid: anyone could suppress a genuine carrier by
 * submitting it first in a different BEEF, and a token refused because the
 * host's headers lagged would stay refused. So every refusal is raised, and
 * the engine records nothing and marks nothing spent: a refused transaction
 * offered again is decided again.
 *
 * No rule reads another topic or anything this topic holds: the previous
 * coins decide only which spent outputs are retained.
 */
import type { ChainTracker } from '@bsv/sdk'
import type { AdmittanceInstructions, ModuleHost, TopicManager } from '@lightwebinc/bcommon'
import { admit, type TxKind } from './admit.js'
import { DefaultMaxBEEF, DefaultMaxSweepInputs } from './beef.js'
import { Refusal, TopicPrefix, checkHall, type Reason } from './borgrec.js'
import { toHex } from './util.js'

/**
 * Every label a host counts a refused submission by: the record rules of
 * spec section 3.1 (a token's record is read by its tag, so even `magic` is
 * reachable), the grant's content rules, and the transaction rules of
 * section 11.1. The member's labels (`token`, `undecryptable`,
 * `commitment`), the grant's field-value labels (`scope`, `expiry`, `role`,
 * which admission reports as `grant-reveal`) and `query` are never an
 * admission verdict.
 */
export const Reasons: readonly Reason[] = [
  'too-large',
  'cbor',
  'key-type',
  'magic',
  'missing',
  'type',
  'range',
  'identity',
  'hall',
  'name',
  'list',
  'chain',
  'members',
  'fields',
  'signature-form',
  'entry',
  'role',
  'permission',
  'binding',
  'count',
  'grant-revocation',
  'grant-signature',
  'grant-reveal',
  'transition',
  'rotation',
  'shape',
  'beef',
  'mineable',
  'unlock',
  'lock',
  'signature',
  'funding',
  'unmined',
  'token-input',
  'not-borg',
]

/** What an admitted transaction was. */
export const AdmitKinds: readonly TxKind[] = ['grant', 'policy', 'wrap', 'org-token', 'group-token', 'sweep']

/** How many admitted txids a manager remembers to count each once. */
export const RecentAdmissions = 4096

/**
 * Raised for every refusal. The engine counts it as a failed topic for this
 * submission and stores nothing.
 */
export class Refused extends Error {
  constructor(
    readonly reason: Reason,
    detail: string,
  ) {
    super(`borg: refused, ${reason}: ${detail}`)
    this.name = 'Refused'
  }
}

export class BorgTopicManager implements TopicManager {
  readonly hall: string

  constructor(
    readonly topic: string,
    private readonly host: ModuleHost,
    private readonly headers: ChainTracker,
    private readonly maxBEEF: number = DefaultMaxBEEF,
    private readonly maxSweepInputs: number = DefaultMaxSweepInputs,
  ) {
    if (!topic.startsWith(TopicPrefix)) throw new Error(`borg: ${topic} is not a tm_borg_ topic`)
    this.hall = topic.slice(TopicPrefix.length)
    checkHall(this.hall)
    // The floors (262144 bytes, 1024 inputs) are held where an operator sets
    // the bounds (module.ts); a test may name the vectors' smaller ones.
    if (!Number.isSafeInteger(maxBEEF) || maxBEEF < 1) throw new Error(`borg: a BEEF bound of ${maxBEEF}`)
    if (!Number.isSafeInteger(maxSweepInputs) || maxSweepInputs < 1) throw new Error(`borg: a sweep input bound of ${maxSweepInputs}`)
  }

  async identifyAdmissibleOutputs(beef: number[], previousCoins: number[]): Promise<AdmittanceInstructions> {
    try {
      const a = await admit(beef, previousCoins, { hall: this.hall, headers: this.headers, maxBEEF: this.maxBEEF, maxSweepInputs: this.maxSweepInputs })
      this.count(a.kind, toHex(a.txid))
      return { outputsToAdmit: a.outputs, coinsToRetain: a.retain }
    } catch (e) {
      if (!(e instanceof Refusal)) throw e
      this.host.metrics.inc('borg_refused_total', { reason: e.reason })
      this.host.log('tm_borg refused', { topic: this.topic, reason: e.reason, detail: e.message })
      throw new Refused(e.reason, e.message.replace(/^borgrec: /, ''))
    }
  }

  /**
   * Counts an admission once per transaction. The engine shows a manager a
   * txid it already applied only when two submissions of it race past its
   * duplicate check, so `borg_admitted_total` counts distinct transactions
   * and `borg_admitted_repeats_total` the rest. The memory is the last
   * RecentAdmissions txids.
   */
  private count(kind: TxKind, txid: string): void {
    if (this.recent.has(txid)) {
      this.host.metrics.inc('borg_admitted_repeats_total', { kind })
      return
    }
    this.recent.add(txid)
    if (this.recent.size > RecentAdmissions) this.recent.delete(this.recent.values().next().value as string)
    this.host.metrics.inc('borg_admitted_total', { kind })
  }

  private readonly recent = new Set<string>()

  /** Nothing is needed: a carrier carries its funding tree, a token its parent, and a sweep names what it spends. */
  async identifyNeededInputs(): Promise<Array<{ txid: string; outputIndex: number }>> {
    return []
  }

  async getDocumentation(): Promise<string> {
    return [
      `# ${this.topic}`,
      '',
      `The borg hall ${this.hall}: grant and wrap carriers, each spending one output of`,
      "its organization's mined funding tree; organization and group tokens, each mined",
      'and published with its parent; and the tombstones of mined sweeps (spec section',
      '11.1). Each is admitted on its own bytes and the host\'s headers; nothing the topic',
      'holds is read. Every refusal is raised, so nothing refused is recorded.',
    ].join('\n')
  }

  async getMetaData(): Promise<{ name: string; shortDescription: string }> {
    return { name: this.topic, shortDescription: `Admits the borg hall ${this.hall}.` }
  }
}
