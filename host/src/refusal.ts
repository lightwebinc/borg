/**
 * The refusal labels of the borg contract and the error that carries one,
 * in a module of their own so that every codec module can throw a refusal
 * without importing the record codecs.
 */
/**
 * The fixed labels a host counts refusals by: the record reasons of spec
 * section 3.1, the content and member reasons, and the transaction reasons
 * of section 11.1.
 */
export type Reason =
  | 'too-large'
  | 'cbor'
  | 'key-type'
  | 'magic'
  | 'missing'
  | 'type'
  | 'range'
  | 'identity'
  | 'hall'
  | 'name'
  | 'list'
  | 'chain'
  | 'members'
  | 'fields'
  | 'signature-form'
  | 'entry'
  | 'grant-revocation'
  | 'grant-signature'
  | 'grant-reveal'
  | 'transition'
  | 'rotation'
  | 'count'
  | 'scope'
  | 'expiry'
  | 'role'
  | 'permission'
  | 'binding'
  | 'query'
  | 'token'
  | 'undecryptable'
  | 'commitment'
  | 'shape'
  | 'beef'
  | 'mineable'
  | 'unlock'
  | 'lock'
  | 'signature'
  | 'funding'
  | 'unmined'
  | 'token-input'
  | 'not-borg'

/** A refusal: the reason label, and a detail for people. */
export class Refusal extends Error {
  constructor(
    readonly reason: Reason,
    detail?: string,
  ) {
    super(detail === undefined ? `borgrec: ${reason}` : `borgrec: ${reason}: ${detail}`)
    this.name = 'Refusal'
  }
}

/** The reason of a thrown value: a Refusal's label, 'other' for anything else. */
export function reasonOf(e: unknown): Reason | 'other' {
  return e instanceof Refusal ? e.reason : 'other'
}
