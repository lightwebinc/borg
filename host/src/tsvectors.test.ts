/**
 * The TypeScript-written vectors regenerate byte for byte, and the
 * TypeScript codec admits every transaction it built.
 */
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import { admit } from './admit.js'
import { file, generate } from './tsvectors.js'
import { fromHex } from './util.js'

test('ts-v1.json regenerates byte for byte', async () => {
  assert.equal(await generate(), readFileSync(file, 'utf8'))
})

test('ts-v1.json: every transaction the TypeScript side built is admitted by it', async () => {
  const v = JSON.parse(readFileSync(file, 'utf8'))
  const roots = new Map<number, string>(v.headers.map((h: { height: number; merkleRoot: string }) => [h.height, h.merkleRoot]))
  const headers = { isValidRootForHeight: async (r: string, h: number) => roots.get(h) === r, currentHeight: async () => 0 }
  for (const t of v.transactions) {
    const a = await admit(fromHex(t.beef), [], { hall: v.hall, headers })
    assert.equal(a.kind, t.kind, t.name)
  }
})
