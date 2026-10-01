/**
 * The lookup question classes of ls_borg (spec section 10.2), validated as
 * parsed: the twin of the Go internal/borgrec/query.go.
 */
import { strictPublicKey } from '@lightwebinc/bcommon'
import { GroupIDLen, MaxEpoch, MaxSafe, Refusal, checkHall } from './borgrec.js'
import { fromHex, isLowerHex } from './util.js'

export interface Class {
  name: string
  /** Sorted. */
  members: string[]
  free: boolean
  /** The most outputs one answer holds. */
  limit: number
}

export const Classes: Class[] = [
  { name: 'org', members: ['hall', 'org'], free: true, limit: 8 },
  { name: 'org-seq', members: ['hall', 'org', 'seq'], free: true, limit: 8 },
  { name: 'roll', members: ['hall', 'roll'], free: true, limit: 64 },
  { name: 'roll-after', members: ['after', 'hall', 'roll'], free: true, limit: 64 },
  { name: 'member', members: ['hall', 'member', 'org'], free: true, limit: 8 },
  { name: 'groups', members: ['groupsOf', 'hall'], free: true, limit: 64 },
  { name: 'groups-after', members: ['after', 'groupsOf', 'hall'], free: true, limit: 64 },
  { name: 'member-of', members: ['hall', 'memberOf', 'org'], free: true, limit: 64 },
  { name: 'member-of-after', members: ['after', 'hall', 'memberOf', 'org'], free: true, limit: 64 },
  { name: 'group', members: ['group', 'hall', 'org'], free: true, limit: 8 },
  { name: 'group-seq', members: ['group', 'hall', 'org', 'seq'], free: true, limit: 8 },
  { name: 'wraps', members: ['epoch', 'group', 'hall', 'org'], free: true, limit: 64 },
  { name: 'wraps-after', members: ['after', 'epoch', 'group', 'hall', 'org'], free: true, limit: 64 },
  { name: 'wrap', members: ['epoch', 'group', 'hall', 'member', 'org'], free: true, limit: 8 },
  { name: 'sweep', members: ['hall', 'spent'], free: true, limit: 8 },
  { name: 'history', members: ['chain', 'hall', 'history'], free: false, limit: 64 },
  { name: 'history-after', members: ['after', 'chain', 'hall', 'history'], free: false, limit: 64 },
]

export interface Question {
  class: Class
  members: Record<string, string>
}

const keyHex = (s: string): boolean => isLowerHex(s, 66) && strictPublicKey(fromHex(s)) !== undefined

/** A decimal from lo to hi, no sign, no leading zero, at most 16 digits. */
function decimal(s: string, lo: number, hi: number): boolean {
  if (s === '' || s.length > 16 || (s.length > 1 && s[0] === '0') || !/^[0-9]+$/.test(s)) return false
  const v = Number(s)
  return v >= lo && v <= hi
}

function member(cls: string, name: string, v: string): boolean {
  switch (name) {
    case 'hall':
      try {
        checkHall(v)
        return true
      } catch {
        return false
      }
    case 'org':
    case 'roll':
    case 'member':
    case 'groupsOf':
    case 'memberOf':
    case 'history':
      return keyHex(v)
    case 'group':
      return isLowerHex(v, 2 * GroupIDLen)
    case 'chain':
      return v === 'org' || isLowerHex(v, 2 * GroupIDLen)
    case 'seq':
      return v === '0' || decimal(v, 1, MaxSafe)
    case 'epoch':
      return v === '0' || decimal(v, 1, MaxEpoch)
    case 'spent': {
      const at = v.indexOf('.')
      return at >= 0 && isLowerHex(v.slice(0, at), 64) && decimal(v.slice(at + 1), 0, MaxEpoch)
    }
    case 'after':
      if (cls === 'roll-after' || cls === 'wraps-after') return isLowerHex(v, 64)
      if (cls === 'groups-after' || cls === 'member-of-after') return isLowerHex(v, 2 * GroupIDLen)
      if (cls === 'history-after') return v === '0' || decimal(v, 1, MaxSafe)
      return false
  }
  return false
}

/**
 * Validates a BRC-24 query object as parsed: a JSON object whose members are
 * all strings (a member given twice takes its last value), whose member set
 * is exactly one class's, and whose values meet their rules.
 */
export function parseQuery(raw: string | unknown): Question {
  let q: unknown = raw
  if (typeof raw === 'string') {
    try {
      q = JSON.parse(raw)
    } catch {
      throw new Refusal('query', 'not a JSON object')
    }
  }
  if (q === null || typeof q !== 'object' || Array.isArray(q)) throw new Refusal('query', 'not a JSON object')
  const vals: Record<string, string> = {}
  for (const [k, v] of Object.entries(q)) {
    if (typeof v !== 'string') throw new Refusal('query', `member ${JSON.stringify(k)} is not a string`)
    vals[k] = v
  }
  const names = Object.keys(vals).sort()
  for (const c of Classes) {
    if (c.members.join(',') !== names.join(',')) continue
    for (const n of names) if (!member(c.name, n, vals[n]!)) throw new Refusal('query', `member ${JSON.stringify(n)}`)
    return { class: c, members: vals }
  }
  throw new Refusal('query', 'no class has these members')
}
