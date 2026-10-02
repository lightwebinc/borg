/**
 * Script reading, the classifier and canonical script rebuilding, the twin
 * of the Go internal/borgrec/script.go and txscript.go. The reader is this
 * package's own rather than the SDK's, so that the Go and TypeScript codecs
 * read every byte string the same way; every script a host checks is
 * compared byte for byte with the one rebuilt here.
 */
import { bytesEqual, claimsRecord, firstPush, pushDropScript } from '@lightwebinc/bcommon'

export { firstPush, minimalPushBytes, pushDropScript, pushFields } from '@lightwebinc/bcommon'
import { MagicGrant, MagicWrap, TagFunding, TagGroup, TagOrg } from './borgrec.js'

const OP_DROP = 0x75

/** What a locking script claims to be (spec section 11.1, Classify). */
export type Kind = 'none' | 'grant' | 'wrap' | 'org-token' | 'group-token' | 'funding'

/** What a locking script claims, reading nothing else of it. */
export function classify(s: Uint8Array): Kind {
  const p = firstPush(s)
  if (p === undefined) return 'none'
  if (claimsRecord(p, MagicGrant)) return 'grant'
  if (claimsRecord(p, MagicWrap)) return 'wrap'
  if (bytesEqual(p, TagOrg)) return 'org-token'
  if (bytesEqual(p, TagGroup)) return 'group-token'
  if (bytesEqual(p, TagFunding) && s.length === 35 + 4 + 1 && s[39] === OP_DROP) return 'funding'
  return 'none'
}

/** A funding-shaped script under any key. */
export const isFundingShape = (s: Uint8Array): boolean => classify(s) === 'funding'

/** A funding output locked to key: <key> OP_CHECKSIG <62 6f 02> OP_DROP. */
export const fundingScript = (key: Uint8Array): Uint8Array => pushDropScript(key, [TagFunding])
