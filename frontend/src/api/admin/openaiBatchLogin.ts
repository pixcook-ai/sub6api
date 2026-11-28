/**
 * Admin OpenAI batch "email----password----2FA" OAuth login API.
 * Secrets are only sent in the start request and are never stored client-side
 * beyond the textarea the operator is typing in.
 */
import { apiClient } from '../client'

export type BatchLoginTarget = 'bps' | 'codex'
export type BatchLoginJobStatus = 'running' | 'done' | 'canceled'
export type BatchLoginItemStatus =
  | 'pending'
  | 'running'
  | 'created'
  | 'skipped'
  | 'failed'
  | 'canceled'

export interface BatchLoginItem {
  email: string
  target: BatchLoginTarget
  status: BatchLoginItemStatus
  stage?: string
  code?: string
  account_id?: number
}

export interface BatchLoginJob {
  id: string
  status: BatchLoginJobStatus
  items: BatchLoginItem[]
}

export interface BatchLoginRequest {
  text: string
  targets: BatchLoginTarget[]
  proxy_id?: number | null
  concurrency?: number
  priority?: number
  bps_group_ids?: number[]
  codex_group_ids?: number[]
  save_credentials?: boolean
}

export const BATCH_LOGIN_MAX_ROWS = 100

export type BatchLoginParseError = 'empty' | 'too_many' | 'invalid_row' | 'duplicate_email'

export class BatchLoginParseFailure extends Error {
  constructor(public readonly code: BatchLoginParseError) {
    super(code)
    this.name = 'BatchLoginParseFailure'
  }
}

export interface BatchLoginRow {
  email: string
  password: string
  totp: string
}

const EMAIL_PATTERN = /^[^\s@]+@[^\s@]+\.[^\s@]+$/
const TOTP_PATTERN = /^[A-Z2-7]+=*$/

/**
 * Strict parser: every non-empty line must be exactly three "----" separated
 * fields. Any malformed line rejects the whole batch so no account is silently
 * dropped. The error carries only a fixed code, never line content.
 */
export function parseBatchLoginText(raw: string): BatchLoginRow[] {
  const lines = raw
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter(Boolean)
  if (!lines.length) throw new BatchLoginParseFailure('empty')
  if (lines.length > BATCH_LOGIN_MAX_ROWS) throw new BatchLoginParseFailure('too_many')
  const seen = new Set<string>()
  return lines.map((line) => {
    const parts = line.split('----')
    if (parts.length !== 3) throw new BatchLoginParseFailure('invalid_row')
    const email = parts[0].trim().toLowerCase()
    const password = parts[1].trim()
    const totp = parts[2].replace(/\s+/g, '').toUpperCase()
    if (
      !EMAIL_PATTERN.test(email) ||
      email.length > 320 ||
      !password ||
      password.length > 4096 ||
      !totp ||
      totp.length > 4096 ||
      !TOTP_PATTERN.test(totp)
    ) {
      throw new BatchLoginParseFailure('invalid_row')
    }
    if (seen.has(email)) throw new BatchLoginParseFailure('duplicate_email')
    seen.add(email)
    return { email, password, totp }
  })
}

export function isBatchLoginJobFinished(job: Pick<BatchLoginJob, 'status'>): boolean {
  return job.status !== 'running'
}

const endpoint = '/admin/openai/batch-login'

export async function startBatchLogin(request: BatchLoginRequest): Promise<BatchLoginJob> {
  const { data } = await apiClient.post<BatchLoginJob>(endpoint, request)
  return data
}

export async function getBatchLogin(id: string): Promise<BatchLoginJob> {
  const { data } = await apiClient.get<BatchLoginJob>(`${endpoint}/${encodeURIComponent(id)}`)
  return data
}

export async function cancelBatchLogin(id: string): Promise<void> {
  await apiClient.delete(`${endpoint}/${encodeURIComponent(id)}`)
}

export async function getLoginCredentialStatus(accountId: number): Promise<{ saved: boolean }> {
  const { data } = await apiClient.get<{ saved: boolean }>(
    `/admin/openai/accounts/${accountId}/login-credential`
  )
  return data
}

export async function deleteLoginCredential(accountId: number): Promise<void> {
  await apiClient.delete(`/admin/openai/accounts/${accountId}/login-credential`)
}

export default {
  startBatchLogin,
  getBatchLogin,
  cancelBatchLogin,
  getLoginCredentialStatus,
  deleteLoginCredential
}
