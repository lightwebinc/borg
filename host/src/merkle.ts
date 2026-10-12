/**
 * The RFC 6962 roots borg commits to (bcommon commit), the roll and members
 * roots built on them, and the inclusion context of a member answer: the
 * twins of the Go crypto.go and context.go.
 */
import { Hash } from '@bsv/sdk'
import { CborError, compareBytes, decodeValue, encode, type Value } from '@lightwebinc/bcommon'
import { MaxRoll, Refusal } from './borgrec.js'
import { bytesEq, concat } from './util.js'

const sha256 = (b: Uint8Array): Uint8Array => Uint8Array.from(Hash.sha256(Array.from(b)))
const leafHash = (d: Uint8Array): Uint8Array => sha256(concat([Uint8Array.of(0), d]))
const nodeHash = (l: Uint8Array, r: Uint8Array): Uint8Array => sha256(concat([Uint8Array.of(1), l, r]))

function split(n: number): number {
  let k = 1
  while (k * 2 < n) k *= 2
  return k
}

function subtree(leaves: Uint8Array[]): Uint8Array {
  if (leaves.length === 1) return leafHash(leaves[0]!)
  const k = split(leaves.length)
  return nodeHash(subtree(leaves.slice(0, k)), subtree(leaves.slice(k)))
}

/** The RFC 6962 Merkle Tree Hash; the empty tree's root is SHA-256 of nothing. */
export function root(leaves: Uint8Array[]): Uint8Array {
  return leaves.length === 0 ? sha256(new Uint8Array(0)) : subtree(leaves)
}

/** One step of an inclusion path: the sibling, and whether it sits on the left. */
export interface Step {
  hash: Uint8Array
  left: boolean
}

export function prove(leaves: Uint8Array[], index: number): Step[] {
  if (leaves.length === 1) return []
  const k = split(leaves.length)
  if (index < k) return [...prove(leaves.slice(0, k), index), { hash: subtree(leaves.slice(k)), left: false }]
  return [...prove(leaves.slice(k), index - k), { hash: subtree(leaves.slice(0, k)), left: true }]
}

export function verify(leaf: Uint8Array, path: Step[], want: Uint8Array): boolean {
  let cur = leafHash(leaf)
  for (const s of path) cur = s.left ? nodeHash(s.hash, cur) : nodeHash(cur, s.hash)
  return bytesEq(cur, want)
}

const sorted = (a: Uint8Array[]): Uint8Array[] => [...a].sort(compareBytes)

/** The members root: leaves SHA-256 of each key, keys ascending. */
export const membersRoot = (keys: Uint8Array[]): Uint8Array => root(sorted(keys).map(sha256))

/** The roll root: leaves the grant commitments, ascending. */
export const rollRoot = (commitments: Uint8Array[]): Uint8Array => root(sorted(commitments))

/**
 * The root an organization record commits its policy set to, computed
 * exactly as rollRoot is over the grant commitments. The two roots are
 * separate so that a change of membership and a change of policy are told
 * apart, and one chain orders both.
 */
export const policyRoot = (commitments: Uint8Array[]): Uint8Array => rollRoot(commitments)

export const MaxPathSteps = 32
export const MaxContext = 2048

/** A member answer's BRC-24 context: [seq, index, size, [[bytes(32), bool], ...]]. */
export interface InclusionContext {
  seq: number
  index: number
  size: number
  path: Step[]
}

export function encodeContext(c: InclusionContext): Uint8Array {
  if (c.path.length > MaxPathSteps || c.size === 0 || c.index >= c.size) throw new Refusal('range')
  return encode([BigInt(c.seq), BigInt(c.index), BigInt(c.size), c.path.map((s) => [s.hash, s.left] as Value[])])
}

export function decodeContext(b: Uint8Array): InclusionContext {
  if (b.length > MaxContext) throw new Refusal('too-large')
  let v: Value
  try {
    v = decodeValue(b)
  } catch (e) {
    if (e instanceof CborError) throw new Refusal('cbor', e.message)
    throw e
  }
  if (!Array.isArray(v) || v.length !== 4) throw new Refusal('type')
  const [seq, index, size, steps] = v
  if (typeof seq !== 'bigint' || typeof index !== 'bigint' || typeof size !== 'bigint' || seq < 0n || index < 0n || size < 0n || !Array.isArray(steps)) {
    throw new Refusal('type')
  }
  if (size === 0n || size > BigInt(MaxRoll) || index >= size || steps.length > MaxPathSteps) throw new Refusal('range')
  const path = steps.map((s) => {
    if (!Array.isArray(s) || s.length !== 2) throw new Refusal('type')
    const [h, left] = s
    if (!(h instanceof Uint8Array) || typeof left !== 'boolean' || h.length !== 32) throw new Refusal('type')
    return { hash: h, left }
  })
  return { seq: Number(seq), index: Number(index), size: Number(size), path }
}

/** The context for the grant at commitment within a roll. */
export function proveGrant(seq: number, roll: Uint8Array[], grant: Uint8Array): InclusionContext {
  const s = sorted(roll)
  const i = s.findIndex((c) => bytesEq(c, grant))
  if (i < 0) throw new Refusal('range', 'not in the roll')
  return { seq, index: i, size: s.length, path: prove(s, i) }
}

/** Whether a context takes the grant's commitment to the roll root. */
export const verifyGrant = (c: InclusionContext, grant: Uint8Array, rollRootValue: Uint8Array): boolean => verify(grant, c.path, rollRootValue)

/**
 * The same check against a policy root: the context a role or bound answer
 * carries has a grant context's shape, and only the root it proves against
 * differs.
 */
export const verifyPolicy = (c: InclusionContext, policy: Uint8Array, policyRootValue: Uint8Array): boolean => verifyGrant(c, policy, policyRootValue)

/**
 * The context for a policy record within a policy set: a grant context's
 * shape, over the other root.
 */
export const provePolicy = (seq: number, set: Uint8Array[], policy: Uint8Array): InclusionContext => proveGrant(seq, set, policy)
