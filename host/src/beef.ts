/**
 * The BEEF as declared on the wire: the shared reader.
 */
export {
  DefaultMaxBEEF,
  anyTxidOnly,
  mergedPath,
  minimalPath,
  readWire,
  subjectTx,
  tokenBEEF,
  type WireEntry as BeefEntry,
  type Wire as WireBEEF,
} from '@lightwebinc/bcommon'

/** The host's sweep input bound when it names none. */
export const DefaultMaxSweepInputs = 1024
