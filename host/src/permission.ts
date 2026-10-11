/**
 * The permission grammar and the role-name grammar of spec section 5A, the
 * TypeScript twin of Go's borgrec/permission.go. Both reach the same verdict
 * on every case in testdata/vectors/permission-v1.json.
 */
import { Refusal } from './refusal.js'

/**
 * Permission bounds. The grammar's longest permission is
 * MaxService + 1 + MaxAction + 1 + MaxResource + 1 = 243 bytes;
 * MaxPermission is the bound a reader holds the text to before it parses.
 */
export const MaxPermission = 256
export const MaxService = 16
export const MaxAction = 32
export const MaxResource = 192
/** The permissions one policy record carries. */
export const MaxPermissions = 24
/** A role name, the same bound as a grant's role field. */
export const MaxRoleName = 64

/**
 * One parsed permission. `prefix` is set when the resource ended in "*",
 * which makes it a prefix rather than a whole resource.
 *
 *   permission = service "/" action "/" resource
 *   service    = 1*16( lower / DIGIT / "-" )
 *   action     = 1*32( lower / DIGIT / "-" )
 *   resource   = "*" / ( 1*192( lower / DIGIT / "-" / "_" / "." / "/" / ":" ) [ "*" ] )
 *
 * The grammar is ASCII and lower case, so no two permissions can look alike
 * and every implementation compares them as bytes. A service and an action
 * are never "*", so no permission is everything everywhere.
 */
export interface Permission {
  service: string
  action: string
  resource: string
  prefix: boolean
}

const lowerDigit = (c: string): boolean => (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
const labelByte = (c: string): boolean => lowerDigit(c) || c === '-'
const resourceByte = (c: string): boolean => labelByte(c) || c === '_' || c === '.' || c === '/' || c === ':'

function run(s: string, max: number, ok: (c: string) => boolean): boolean {
  if (s.length < 1 || s.length > max) return false
  for (const c of s) if (!ok(c)) return false
  return true
}

/**
 * The grammar of spec section 5A.2. A service and an action hold no "/", so
 * the first two separators split the three parts even where the resource
 * holds further ones.
 */
export function parsePermission(s: string): Permission {
  if (s.length > MaxPermission) throw new Refusal('permission', s)
  const i = s.indexOf('/')
  if (i < 0) throw new Refusal('permission', s)
  const j = s.indexOf('/', i + 1)
  if (j < 0) throw new Refusal('permission', s)
  const service = s.slice(0, i)
  const action = s.slice(i + 1, j)
  let resource = s.slice(j + 1)
  if (!run(service, MaxService, labelByte) || !run(action, MaxAction, labelByte)) throw new Refusal('permission', s)
  if (resource === '*') return { service, action, resource: '', prefix: true }
  let prefix = false
  if (resource.endsWith('*')) {
    prefix = true
    resource = resource.slice(0, -1)
  }
  if (!run(resource, MaxResource, resourceByte)) throw new Refusal('permission', s)
  return { service, action, resource, prefix }
}

/** The one spelling a reader accepted: parsePermission(write(p)) === p. */
export const write = (p: Permission): string => `${p.service}/${p.action}/${p.resource}${p.prefix ? '*' : ''}`

/**
 * Whether the held permission grants the asked one. The service and the
 * action always match exactly; a held resource that ended in "*" matches by
 * prefix, and nothing else matches: there is no suffix wildcard, none inside
 * a resource, and no hierarchy a separator implies.
 */
export function grantsPermission(held: Permission, asked: Permission): boolean {
  if (held.service !== asked.service || held.action !== asked.action || asked.prefix) return false
  return held.prefix ? asked.resource.startsWith(held.resource) : held.resource === asked.resource
}

/**
 * A permission a service asks about, stricter than parsePermission: a "*"
 * anywhere in it is a programming error, so it is refused rather than
 * treated as a character or as a wildcard.
 */
export function asked(s: string): Permission {
  const p = parsePermission(s)
  if (p.prefix) throw new Refusal('permission', s)
  return p
}

/**
 * Whether any held permission grants the asked one. A held member that
 * breaks the grammar is refused, never a quiet miss: a reader that met one
 * has read a record no host admits.
 */
export function grantsAny(held: string[], ask: string): boolean {
  const a = asked(ask)
  for (const h of held) if (grantsPermission(parsePermission(h), a)) return true
  return false
}

/**
 * The role-name grammar of spec section 5A.1: 1 to 64 of lower-case ASCII
 * letters, digits, "-", "_" and ".", the characters BRC-169 section 9.2
 * allows a scope qualifier. A role name is an identifier compared byte for
 * byte, so it carries no case and no character a renderer would strip.
 */
export function checkRoleName(s: string): void {
  if (!run(s, MaxRoleName, (c) => lowerDigit(c) || c === '-' || c === '_' || c === '.')) {
    throw new Refusal('role', 'role name breaks the grammar')
  }
}

/**
 * The permissions of a that b also carries, in a's order. Attenuating a
 * binding is this over the role's set, so a binding can never widen a role,
 * and two definitions of one role are read as this over both.
 */
export const intersect = (a: string[], b: string[]): string[] => a.filter((x) => b.includes(x))
