import { describe, it, expect } from 'vitest'
import { CODEX_PERMISSION_PROFILES, CUSTOM_PROFILE, pairForProfile, profileForPair } from './codex_permissions.js'

describe('codex permission profiles', () => {
  it("speaks codex's own profile ids and maps each onto the wire pair", () => {
    expect(CODEX_PERMISSION_PROFILES.map((p) => p.id)).toEqual([':read-only', ':workspace', ':danger-full-access'])
    expect(pairForProfile(':read-only')).toEqual({ sandbox: 'read-only', approval: 'never' })
    expect(pairForProfile(':workspace')).toEqual({ sandbox: 'workspace-write', approval: 'never' })
    expect(pairForProfile(':danger-full-access')).toEqual({ sandbox: 'danger-full-access', approval: 'never' })
  })

  it('round-trips: every profile is recovered from its own pair', () => {
    for (const p of CODEX_PERMISSION_PROFILES) {
      expect(profileForPair(p.sandbox, p.approval)).toBe(p.id)
    }
  })

  // A stored default the profiles do not cover (e.g. approvals on) must
  // surface as Custom and be preserved, never silently rewritten.
  it('answers Custom for a pair outside the table, and leaves it alone', () => {
    expect(profileForPair('read-only', 'on-request')).toBe(CUSTOM_PROFILE)
    expect(profileForPair('workspace-write', 'untrusted')).toBe(CUSTOM_PROFILE)
    expect(pairForProfile(CUSTOM_PROFILE)).toBeNull()
  })
})
