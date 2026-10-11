/**
 * A grant's field values: the scope grammar and its fail-closed meaning, the
 * expiry and the role. The twin of the Go borgrec/scope.go.
 */
import { Refusal } from './refusal.js'

/**
 * The clock skew a reader allows an expiry, in seconds, in either direction
 * (spec section 5.2; BRC-169 section 9.6). It is part of the contract: two
 * readers with clocks within it reach one verdict.
 */
export const ExpirySkew = 300

/** A grant's role and scope bounds, in bytes (spec section 14). */
export const MaxRole = 64
export const MaxScope = 512

export const VerbMember = 'member'
export const VerbAdmin = 'admin'

export interface ScopeToken {
  verb: string
  qualifier?: string
}

const lowerDigit = (c: number): boolean => (c >= 0x61 && c <= 0x7a) || (c >= 0x30 && c <= 0x39)

/** BRC-169 section 9.2's grammar, with the whole-scope "*" refused. */
export function parseScope(s: string): ScopeToken[] {
  if (s.length === 0 || s.length > MaxScope) throw new Refusal('scope')
  const out: ScopeToken[] = []
  for (const p of s.split(',')) {
    const at = p.indexOf(':')
    const verb = at < 0 ? p : p.slice(0, at)
    if (verb.length < 1 || verb.length > 32) throw new Refusal('scope')
    for (let i = 0; i < verb.length; i++) {
      const c = verb.charCodeAt(i)
      if (!lowerDigit(c) && c !== 0x2d) throw new Refusal('scope')
    }
    if (at < 0) {
      out.push({ verb })
      continue
    }
    const qual = p.slice(at + 1)
    if (qual.length < 1 || qual.length > 64) throw new Refusal('scope')
    for (let i = 0; i < qual.length; i++) {
      const c = qual.charCodeAt(i)
      if (!lowerDigit(c) && c !== 0x2d && c !== 0x5f && c !== 0x2e) throw new Refusal('scope')
    }
    out.push({ verb, qualifier: qual })
  }
  return out
}

/** Whether a parsed scope grants verb unqualified; a qualified token for verb denies it. */
export function grants(tokens: ScopeToken[], verb: string): boolean {
  let found = false
  for (const t of tokens) {
    if (t.verb !== verb) continue
    if (t.qualifier !== undefined) return false
    found = true
  }
  return found
}

/** Whether a scope parses and grants "member". */
export function isMembership(scope: string): boolean {
  try {
    return grants(parseScope(scope), VerbMember)
  } catch {
    return false
  }
}

const expiryRe = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})Z$/

/** The expiry rule: exactly YYYY-MM-DDTHH:MM:SSZ, a real UTC time from 1970. Seconds since 1970. */
export function parseExpiry(s: string): number {
  const m = expiryRe.exec(s)
  if (m === null) throw new Refusal('expiry')
  const [y, mo, d, h, mi, se] = m.slice(1).map(Number) as [number, number, number, number, number, number]
  const leap = (y % 4 === 0 && y % 100 !== 0) || y % 400 === 0
  const days = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31]
  if (y < 1970 || mo < 1 || mo > 12 || d < 1 || d > days[mo - 1]! || h > 23 || mi > 59 || se > 59) throw new Refusal('expiry')
  return Date.UTC(y, mo - 1, d, h, mi, se) / 1000
}

/** The role rule: 1 to 64 bytes of UTF-8. */
export function checkRole(b: Uint8Array): void {
  if (b.length < 1 || b.length > MaxRole) throw new Refusal('role')
  try {
    new TextDecoder('utf-8', { fatal: true }).decode(b)
  } catch {
    throw new Refusal('role')
  }
}

/**
 * The rule of a grant field applied to its decrypted bytes. An expiry or a
 * scope is ASCII by its grammar, so a byte above 0x7f refuses it.
 */
export function checkFieldValue(name: string, value: Uint8Array): string {
  if (name === 'role') {
    checkRole(value)
    return new TextDecoder().decode(value)
  }
  const ascii = value.every((c) => c < 0x80)
  const s = ascii ? String.fromCharCode(...value) : ''
  if (name === 'expiry') {
    if (!ascii) throw new Refusal('expiry')
    parseExpiry(s)
    return s
  }
  if (name === 'scope') {
    if (!ascii) throw new Refusal('scope')
    parseScope(s)
    return s
  }
  throw new Refusal('fields', name)
}
