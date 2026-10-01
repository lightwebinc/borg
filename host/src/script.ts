/**
 * Script reading, the classifier and canonical script rebuilding, the twin
 * of the Go internal/borgrec/script.go and txscript.go. The reader is this
 * package's own rather than the SDK's, so that the Go and TypeScript codecs
 * read every byte string the same way; every script a host checks is
 * compared byte for byte with the one rebuilt here.
 */
import { bytesEqual } from '@lightwebinc/bcommon'
import { MagicGrant, MagicWrap, TagFunding, TagGroup, TagOrg } from './borgrec.js'
import { concat } from './util.js'

const OP_0 = 0x00
const OP_PUSHDATA1 = 0x4c
const OP_PUSHDATA2 = 0x4d
const OP_PUSHDATA4 = 0x4e
const OP_1NEGATE = 0x4f
const OP_DROP = 0x75
const OP_2DROP = 0x6d
const OP_CHECKSIG = 0xac

/** What a locking script claims to be (spec section 11.1, Classify). */
export type Kind = 'none' | 'grant' | 'wrap' | 'org-token' | 'group-token' | 'funding'

/**
 * The push that follows a lock-before PushDrop's key and OP_CHECKSIG: an
 * opcode 0x01 to 0x4b, or OP_PUSHDATA1, 2 or 4 with its little-endian
 * length, every declared byte present.
 */
export function firstPush(s: Uint8Array): Uint8Array | undefined {
  if (s.length < 36 || s[0] !== 0x21 || s[34] !== OP_CHECKSIG) return undefined
  const p = s.subarray(35)
  const op = p[0]!
  let n: number
  let off: number
  if (op >= 0x01 && op <= 0x4b) {
    n = op
    off = 1
  } else if (op === OP_PUSHDATA1 && p.length >= 2) {
    n = p[1]!
    off = 2
  } else if (op === OP_PUSHDATA2 && p.length >= 3) {
    n = p[1]! | (p[2]! << 8)
    off = 3
  } else if (op === OP_PUSHDATA4 && p.length >= 5) {
    n = (p[1]! | (p[2]! << 8) | (p[3]! << 16)) + p[4]! * 0x1000000
    if (n > p.length) return undefined
    off = 5
  } else {
    return undefined
  }
  if (p.length < off + n) return undefined
  return p.subarray(off, off + n)
}

/** A definite-length map head, key 0, then a four-byte string equal to magic. */
function claimsMagic(p: Uint8Array, magic: Uint8Array): boolean {
  if (p.length < 1 || p[0]! >> 5 !== 5) return false
  const ai = p[0]! & 0x1f
  let i: number
  if (ai < 24) i = 1
  else if (ai <= 27) i = 1 + 2 ** (ai - 24)
  else return false
  if (p.length < i + 2 + magic.length || p[i] !== 0x00 || p[i + 1] !== 0x44) return false
  return bytesEqual(p.subarray(i + 2, i + 2 + magic.length), magic)
}

/** What a locking script claims, reading nothing else of it. */
export function classify(s: Uint8Array): Kind {
  const p = firstPush(s)
  if (p === undefined) return 'none'
  if (claimsMagic(p, MagicGrant)) return 'grant'
  if (claimsMagic(p, MagicWrap)) return 'wrap'
  if (bytesEqual(p, TagOrg)) return 'org-token'
  if (bytesEqual(p, TagGroup)) return 'group-token'
  if (bytesEqual(p, TagFunding) && s.length === 35 + 4 + 1 && s[39] === OP_DROP) return 'funding'
  return 'none'
}

/** A funding-shaped script under any key. */
export const isFundingShape = (s: Uint8Array): boolean => classify(s) === 'funding'

/** One parsed script element; data is set for a data push. */
interface Op {
  code: number
  data?: Uint8Array
}

/** Splits s into opcodes; a push running past the end refuses the whole script. */
function parseScript(s: Uint8Array): Op[] | undefined {
  const out: Op[] = []
  let i = 0
  while (i < s.length) {
    const c = s[i++]!
    let n: number
    if (c === OP_0) {
      out.push({ code: c, data: new Uint8Array(0) })
      continue
    } else if (c >= 1 && c <= 75) {
      n = c
    } else if (c === OP_PUSHDATA1) {
      if (i + 1 > s.length) return undefined
      n = s[i]!
      i += 1
    } else if (c === OP_PUSHDATA2) {
      if (i + 2 > s.length) return undefined
      n = s[i]! | (s[i + 1]! << 8)
      i += 2
    } else if (c === OP_PUSHDATA4) {
      if (i + 4 > s.length) return undefined
      n = (s[i]! | (s[i + 1]! << 8) | (s[i + 2]! << 16)) + s[i + 3]! * 0x1000000
      i += 4
    } else {
      out.push({ code: c })
      continue
    }
    if (n > s.length - i) return undefined
    out.push({ code: c, data: s.subarray(i, i + n) })
    i += n
  }
  return out
}

/**
 * A lock-before PushDrop read leniently: a 33-byte push, OP_CHECKSIG, then
 * every data push up to the first opcode that is not one (the field
 * signature included). Canonicality is the rebuild's.
 */
export function pushFields(s: Uint8Array): Uint8Array[] | undefined {
  const ops = parseScript(s)
  if (ops === undefined || ops.length < 3 || ops[0]!.data?.length !== 33 || ops[1]!.code !== OP_CHECKSIG) return undefined
  const fields: Uint8Array[] = []
  for (const o of ops.slice(2)) {
    if (o.data === undefined) break
    fields.push(o.data)
  }
  return fields.length > 0 ? fields : undefined
}

/** The minimal push of d, as the SDKs write it. */
export function minimalPushBytes(d: Uint8Array): Uint8Array {
  const n = d.length
  if (n === 0 || (n === 1 && d[0] === 0)) return Uint8Array.of(OP_0)
  if (n === 1 && d[0]! >= 1 && d[0]! <= 16) return Uint8Array.of(0x50 + d[0]!)
  if (n === 1 && d[0] === 0x81) return Uint8Array.of(OP_1NEGATE)
  let head: number[]
  if (n <= 75) head = [n]
  else if (n <= 255) head = [OP_PUSHDATA1, n]
  else if (n <= 65535) head = [OP_PUSHDATA2, n & 0xff, n >> 8]
  else head = [OP_PUSHDATA4, n & 0xff, (n >> 8) & 0xff, (n >> 16) & 0xff, (n >>> 24) & 0xff]
  return concat([Uint8Array.from(head), d])
}

/**
 * The one canonical lock-before PushDrop for a 33-byte key, fields and an
 * optional field signature, exactly as bcommon pushdrop writes it.
 */
export function pushDropScript(key: Uint8Array, fields: Uint8Array[], sig?: Uint8Array): Uint8Array {
  const all = sig === undefined ? fields : [...fields, sig]
  const parts: Uint8Array[] = [Uint8Array.of(33), key, Uint8Array.of(OP_CHECKSIG), ...all.map(minimalPushBytes)]
  for (let n = all.length; n > 0; n -= 2) parts.push(Uint8Array.of(n === 1 ? OP_DROP : OP_2DROP))
  return concat(parts)
}

/** A funding output locked to key: <key> OP_CHECKSIG <62 6f 02> OP_DROP. */
export const fundingScript = (key: Uint8Array): Uint8Array => pushDropScript(key, [TagFunding])
