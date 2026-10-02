/**
 * What ls_borg keeps for the host's life (spec sections 11.2 and 11.3), as
 * an append-only journal beside the engine's storage. The engine's storage
 * holds the outputs a host still keeps; it cannot hold what must outlive
 * them: a dropped carrier, which carrier won an outpoint, the sweeps that
 * spent it, and when the host first saw each token.
 *
 * One JSON object a line:
 *
 *   {"k":"c","op":"<txid>.<vout>","c":"<C>","txid":"<txid>","kind":"g","hall":"...","at":<s>}
 *   {"k":"d","op":"...","c":"<C>","w":true}
 *   {"k":"s","op":"...","s":"<sweep txid>","hall":"...","h":<height>,"at":<s>}
 *   {"k":"t","txid":"<txid>","vout":<n>,"hall":"...","at":<s>}
 *   {"k":"w","txid":"<sweep txid>","hall":"..."}
 *
 * `c` lines are every carrier any hall admitted (`kind` `g` a grant, `w` a
 * wrap) with the funding outpoint it spent; `d` lines are drops, `w` when
 * the dropped carrier was its outpoint's winner; `s` lines are admitted
 * sweeps' spends of outpoints a carrier spent (spends of any other outpoint
 * live only in the bounded table, never here); `t` lines are chain tokens; a
 * `w` line names a held sweep whose tombstone an admitted transaction spent,
 * which storage no longer hands a restore among its unspent rows. A journal
 * written before `w` lines existed holds none and loads as it always did.
 * Commitments are in hash byte order and txids in display order, both
 * lowercase hex; `at` is the host's time of first sight in Unix seconds.
 */
import { closeSync, fdatasyncSync, mkdirSync, openSync, readFileSync, truncateSync, writeSync } from 'node:fs'
import { join } from 'node:path'

export interface CarrierLine {
  k: 'c'
  op: string
  c: string
  txid: string
  kind: 'g' | 'w'
  hall: string
  at: number
}

export interface DropLine {
  k: 'd'
  op: string
  c: string
  w: boolean
}

export interface SweepLine {
  k: 's'
  op: string
  s: string
  hall: string
  h: number
  at: number
}

export interface TokenLine {
  k: 't'
  txid: string
  vout: number
  hall: string
  at: number
}

export interface SpentSweepLine {
  k: 'w'
  txid: string
  hall: string
}

export type Line = CarrierLine | DropLine | SweepLine | TokenLine | SpentSweepLine

/** Where the rows live. `append` is durable when it returns. */
export interface Journal {
  load(): Line[]
  append(l: Line): void
}

/** A journal in memory, for tests and for a host that accepts losing it. */
export class MemoryJournal implements Journal {
  readonly lines: Line[] = []
  load(): Line[] {
    return this.lines.map((l) => ({ ...l }))
  }
  append(l: Line): void {
    this.lines.push({ ...l })
  }
}

function isLine(v: unknown): v is Line {
  if (typeof v !== 'object' || v === null) return false
  const o = v as Record<string, unknown>
  const s = (k: string): boolean => typeof o[k] === 'string'
  const n = (k: string): boolean => typeof o[k] === 'number' && Number.isSafeInteger(o[k]) && (o[k] as number) >= 0
  switch (o['k']) {
    case 'c':
      return s('op') && s('c') && s('txid') && (o['kind'] === 'g' || o['kind'] === 'w') && s('hall') && n('at')
    case 'd':
      return s('op') && s('c') && typeof o['w'] === 'boolean'
    case 's':
      return s('op') && s('s') && s('hall') && n('h') && n('at')
    case 't':
      return s('txid') && n('vout') && s('hall') && n('at')
    case 'w':
      return s('txid') && s('hall')
  }
  return false
}

/**
 * The journal as a file, `borg-rows.jsonl` in dir. Each line is written and
 * flushed to disk before append returns. A last line cut short by a crash
 * is cut off on load; any other line that does not parse stops the load,
 * because a row silently skipped could let a swept or superseded carrier be
 * answered again.
 */
export class FileJournal implements Journal {
  readonly path: string
  private fd: number | undefined

  constructor(dir: string) {
    mkdirSync(dir, { recursive: true })
    this.path = join(dir, 'borg-rows.jsonl')
  }

  load(): Line[] {
    let text: string
    try {
      text = readFileSync(this.path, 'utf8')
    } catch (e) {
      if ((e as NodeJS.ErrnoException).code === 'ENOENT') return []
      throw e
    }
    if (text.length > 0 && !text.endsWith('\n')) {
      const keep = text.lastIndexOf('\n') + 1
      truncateSync(this.path, Buffer.byteLength(text.slice(0, keep), 'utf8'))
      text = text.slice(0, keep)
    }
    const out: Line[] = []
    for (const [i, l] of text.split('\n').entries()) {
      if (l === '') continue
      let v: unknown
      try {
        v = JSON.parse(l)
      } catch {
        throw new Error(`${this.path}: line ${i + 1} is not JSON`)
      }
      if (!isLine(v)) throw new Error(`${this.path}: line ${i + 1} is not a borg row`)
      out.push(v)
    }
    return out
  }

  append(l: Line): void {
    this.fd ??= openSync(this.path, 'a')
    writeSync(this.fd, JSON.stringify(l) + '\n')
    fdatasyncSync(this.fd)
  }

  close(): void {
    if (this.fd !== undefined) closeSync(this.fd)
    this.fd = undefined
  }
}
