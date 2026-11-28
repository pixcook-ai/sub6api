import { describe, expect, it } from 'vitest'
import {
  BATCH_LOGIN_MAX_ROWS,
  BatchLoginParseFailure,
  isBatchLoginJobFinished,
  parseBatchLoginText
} from '../admin/openaiBatchLogin'

function codeOf(fn: () => unknown): string | undefined {
  try {
    fn()
  } catch (error) {
    return error instanceof BatchLoginParseFailure ? error.code : 'unexpected'
  }
  return undefined
}

describe('parseBatchLoginText', () => {
  it('parses lines, trims, lowercases email and normalizes the 2FA secret', () => {
    const rows = parseBatchLoginText('A@Example.com----pw1----jbsw y3dp\r\n\r\n c@d.co ---- pw2 ----ABCD234=\n')
    expect(rows).toEqual([
      { email: 'a@example.com', password: 'pw1', totp: 'JBSWY3DP' },
      { email: 'c@d.co', password: 'pw2', totp: 'ABCD234=' }
    ])
  })

  it('rejects empty input', () => {
    expect(codeOf(() => parseBatchLoginText(' \n\r\n '))).toBe('empty')
  })

  it('rejects the whole batch when any line is malformed', () => {
    for (const bad of [
      'a@b.co----pw',
      'a@b.co----pw----ABCD----extra',
      'a@b.co,pw,ABCD',
      'notanemail----pw----ABCD',
      'a@b.co--------ABCD',
      'a@b.co----pw----not base32!'
    ]) {
      expect(codeOf(() => parseBatchLoginText(`ok@b.co----pw----ABCD\n${bad}`))).toBe('invalid_row')
    }
  })

  it('rejects duplicate emails case-insensitively', () => {
    expect(codeOf(() => parseBatchLoginText('a@b.co----p----ABCD\nA@B.co----q----ABCD'))).toBe('duplicate_email')
  })

  it('enforces the row limit', () => {
    const lines = (n: number) =>
      Array.from({ length: n }, (_, i) => `u${i}@x.co----pw----ABCD`).join('\n')
    expect(parseBatchLoginText(lines(BATCH_LOGIN_MAX_ROWS))).toHaveLength(BATCH_LOGIN_MAX_ROWS)
    expect(codeOf(() => parseBatchLoginText(lines(BATCH_LOGIN_MAX_ROWS + 1)))).toBe('too_many')
  })

  it('never puts line content or secrets in the error', () => {
    try {
      parseBatchLoginText('a@b.co----super-secret-pw')
      throw new Error('should have thrown')
    } catch (error) {
      expect(String((error as Error).message)).not.toContain('super-secret-pw')
      expect(String((error as Error).message)).not.toContain('a@b.co')
    }
  })
})

describe('isBatchLoginJobFinished', () => {
  it('is false only while running', () => {
    expect(isBatchLoginJobFinished({ status: 'running' })).toBe(false)
    expect(isBatchLoginJobFinished({ status: 'done' })).toBe(true)
    expect(isBatchLoginJobFinished({ status: 'canceled' })).toBe(true)
  })
})
