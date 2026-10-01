/**
 * What the index computes from admitted objects (spec section 11.2), the
 * twin of the Go internal/borgrec/chain.go: the fold of each chain from seq
 * 0 with each token's status, and a funding outpoint's row with each
 * carrier's status. Never an admission verdict.
 */
import { bytesEqual, compareBytes } from '@lightwebinc/bcommon'
import { beginsEpoch, type Group, type Org } from './borgrec.js'
import { membersRoot, rollRoot } from './merkle.js'
import { toHex } from './util.js'

export type TokenStatus = 'consistent' | 'pending' | 'roll-mismatch' | 'members-mismatch' | 'commitment-reuse' | 'rejoin-unrotated'
export type CarrierStatus = 'retracted' | 'superseded' | 'held'

/** An organisation's roll, folded from seq 0; a broken fold stays broken. */
export class OrgFold {
  private readonly roll = new Map<string, Uint8Array>()
  private broken = false

  apply(o: Org): TokenStatus {
    if (this.broken) return 'roll-mismatch'
    for (const c of o.revoke) {
      if (!this.roll.delete(toHex(c))) return this.break()
    }
    for (const c of o.admit) {
      if (this.roll.has(toHex(c))) return this.break()
      this.roll.set(toHex(c), c)
    }
    if (this.roll.size !== o.count || !bytesEqual(rollRoot([...this.roll.values()]), o.roll)) return this.break()
    return 'consistent'
  }

  private break(): TokenStatus {
    this.broken = true
    return 'roll-mismatch'
  }

  rollNow(): Uint8Array[] {
    return [...this.roll.values()].sort(compareBytes)
  }

  /** Whether the roll holds commitment c (hash byte order hex). */
  holds(c: string): boolean {
    return this.roll.has(c)
  }

  /** A copy to fold a second successor on. */
  clone(): OrgFold {
    const f = new OrgFold()
    for (const [k, v] of this.roll) f.roll.set(k, v)
    f.broken = this.broken
    return f
  }
}

/** A group's members, every removed key and every epoch's commitment, folded from seq 0. */
export class GroupFold {
  private readonly members = new Map<string, Uint8Array>()
  private readonly removed = new Set<string>()
  private readonly commitments = new Set<string>()
  private broken = false

  apply(g: Group): TokenStatus {
    if (this.broken) return 'members-mismatch'
    const rejoin = !beginsEpoch(g) && g.add.some((k) => this.removed.has(toHex(k)))
    for (const k of g.remove) {
      if (!this.members.delete(toHex(k))) return this.break()
    }
    for (const k of g.add) {
      if (this.members.has(toHex(k))) return this.break()
      this.members.set(toHex(k), k)
    }
    if (this.members.size !== g.count || !bytesEqual(membersRoot([...this.members.values()]), g.members)) return this.break()
    for (const k of g.remove) this.removed.add(toHex(k))
    const c = toHex(g.commitment)
    const reuse = beginsEpoch(g) && this.commitments.has(c)
    if (beginsEpoch(g)) this.commitments.add(c)
    if (reuse) return 'commitment-reuse'
    if (rejoin) return 'rejoin-unrotated'
    return 'consistent'
  }

  private break(): TokenStatus {
    this.broken = true
    return 'members-mismatch'
  }

  membersNow(): Uint8Array[] {
    return [...this.members.values()].sort(compareBytes)
  }

  /** Whether key (hex) is a member. */
  holds(key: string): boolean {
    return this.members.has(key)
  }

  /** A copy to fold a second successor on. */
  clone(): GroupFold {
    const f = new GroupFold()
    for (const [k, v] of this.members) f.members.set(k, v)
    for (const k of this.removed) f.removed.add(k)
    for (const c of this.commitments) f.commitments.add(c)
    f.broken = this.broken
    return f
  }
}

/** One funding outpoint's row: its carriers' commitments, whether the winner was dropped, whether it was swept. */
export interface OutpointRow {
  commitments: Uint8Array[]
  winnerDropped: boolean
  swept: boolean
}

/** The lowest commitment, compared as 32 bytes in hash byte order. */
export function winner(r: OutpointRow): Uint8Array | undefined {
  return [...r.commitments].sort(compareBytes)[0]
}

/** retracted, then superseded, then held. */
export function carrierStatus(r: OutpointRow, c: Uint8Array): CarrierStatus {
  if (r.swept) return 'retracted'
  const w = winner(r)
  if (w === undefined || !bytesEqual(w, c) || r.winnerDropped) return 'superseded'
  return 'held'
}
