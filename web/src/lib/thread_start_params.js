
export const STORAGE_KEY = 'nano:threadStartParams:codex'
export const SCHEMA_VERSION = 'codex-cli 0.142.5'
export const SCHEMA_DOC = 'docs/parsing/codex-wire.md'

// Params shown in the primary New Window flow by default; the rest live under
// New Window's "Advanced" section (matches today's NewSessionModal primary set).
const INITIAL_SHOW = new Set(['cwd', 'model', 'effort', 'sandbox', 'approvalPolicy', 'serviceTier'])

// Params with a built-in default until the user changes them.
const INITIAL_DEFAULTS = { model: 'gpt-5.6-sol' }

// kind: how the Default column renders a control.
//   'model'    — select from GET /api/codex/models, free-text fallback
//   'enum'     — <select> with (unset) + options
//   'bool'     — <select> (unset) / true / false
//   'text'     — mono text input
//   'textarea' — 2-row mono textarea
export const PARAMS = [
  {
    name: 'model',
    type: 'string',
    kind: 'model',
    placeholder: 'e.g. gpt-5.6-sol',
    values: 'e.g. `gpt-5.6-sol`',
    notes: 'Omit → config/profile default.',
  },
  {
    name: 'effort',
    type: 'string',
    kind: 'text',
    placeholder: 'model default',
    values: 'model-advertised values (`model/list` `supportedReasoningEfforts`)',
    notes: 'Technically turn-level, not thread/start — nano sends it as a `turn/start` override on every turn (doc §4). Settable here so New Window can fix it at window start. Rendered with the model picker.',
  },
  {
    name: 'serviceTier',
    type: 'string',
    kind: 'enum',
    options: [
      { value: 'default', label: 'default' },
      { value: 'priority', label: 'Fast' },
      { value: 'ultrafast', label: 'Ultrafast (when advertised)' },
    ],
    values: '`default` and model/list `serviceTiers` ids, including `priority` and `ultrafast`',
    notes: 'Standard is Nano\'s default. Use `priority` or `ultrafast` only when explicitly requested; Ultrafast requires the selected model and account to advertise it. Nano resolves omission to `default` because raw app-server omission can inherit a Priority operator config.',
  },
  {
    name: 'modelProvider',
    type: 'string',
    kind: 'text',
    placeholder: '[model_providers] key',
    values: 'key into config `[model_providers]`',
    notes: 'Effectively always OpenAI: only used to point at a custom/OSS endpoint defined under `[model_providers]` in config.toml (e.g. a local `oss` provider). Codex offers no picker — leave unset unless you have defined one.',
  },
  {
    name: 'cwd',
    type: 'string',
    kind: 'text',
    placeholder: 'absolute path',
    values: 'absolute path',
    notes: 'Drives AGENTS.md discovery + sandbox root.',
  },
  {
    name: 'approvalPolicy',
    type: 'enum',
    kind: 'enum',
    options: [
      { value: 'untrusted', label: 'untrusted' },
      { value: 'on-request', label: 'on-request' },
      { value: 'on-failure', label: 'on-failure (deprecated)' },
      { value: 'never', label: 'never' },
    ],
    values: '`untrusted` | `on-failure` | `on-request` | `never`, or a granular object (§3, five independent approval gates)',
    notes: "Codex uses several names for the same thing. TUI presets map to approval+sandbox pairs: Read Only = `on-request` + read-only sandbox; Auto = `on-request` + workspace-write; Full Access ('yolo', CLI `--dangerously-bypass-approvals-and-sandbox`) = `never` + danger-full-access. `untrusted` = ask before anything but known-safe commands. `on-failure` is deprecated (still schema-valid in 0.142.5).",
  },
  {
    name: 'approvalsReviewer',
    type: 'enum',
    kind: 'enum',
    options: [
      { value: 'user', label: 'user' },
      { value: 'auto_review', label: 'auto_review' },
    ],
    values: '`user` | `auto_review` (`guardian_subagent` legacy alias)',
    notes: 'Routes approval requests to human or risk-assessing subagent. Default `user`. `auto_review` routes eligible interactive prompts to the reviewer subagent — it does not expand sandbox/network/file permissions.',
  },
  {
    name: 'sandbox',
    type: 'enum',
    kind: 'enum',
    options: [
      { value: 'read-only', label: 'read-only' },
      { value: 'workspace-write', label: 'workspace-write' },
      { value: 'danger-full-access', label: 'danger-full-access' },
    ],
    values: '`read-only` | `workspace-write` | `danger-full-access`',
    notes: 'Simple 3-value mode. The rich policy object exists only at turn/start (§4).',
  },
  {
    name: 'personality',
    type: 'enum',
    kind: 'enum',
    options: [
      { value: 'none', label: 'none' },
      { value: 'friendly', label: 'friendly' },
      { value: 'pragmatic', label: 'pragmatic' },
    ],
    values: '`none` | `friendly` | `pragmatic`',
    notes: 'Observed default in rollouts: `pragmatic`. Per-model support advertised by model/list `supportsPersonality`; also gated by config `features.personality`.',
  },
  {
    name: 'baseInstructions',
    type: 'string',
    kind: 'textarea',
    placeholder: 'full replacement text',
    values: 'full replacement text',
    notes: 'Replaces the built-in system-prompt layer, NOT host/org policy above it. Config-level twin: `model_instructions_file`.',
  },
  {
    name: 'developerInstructions',
    type: 'string',
    kind: 'textarea',
    placeholder: 'text injected at developer role',
    values: 'text',
    notes: 'Injected at developer role. Config-level twin: `developer_instructions`.',
  },
  {
    name: 'config',
    type: 'object',
    kind: 'textarea',
    placeholder: '{"model_reasoning_effort": "high"}',
    values: 'arbitrary key→value, `additionalProperties: true`',
    notes: 'Escape hatch: per-thread config.toml overrides (CLI `-c` equivalent). E.g. `model_reasoning_effort`, feature flags.',
  },
  {
    name: 'ephemeral',
    type: 'bool',
    kind: 'bool',
    values: 'boolean',
    notes: '`true` → no rollout on disk; thread lives only in the app-server. Also no resume-from-disk.',
  },
  {
    name: 'threadSource',
    type: 'string',
    kind: 'text',
    placeholder: 'free string',
    values: 'free string',
    notes: 'Client-supplied analytics classification. Server still stamps app-server threads `source: "vscode"` in the rollout.',
  },
  {
    name: 'sessionStartSource',
    type: 'enum',
    kind: 'enum',
    options: [
      { value: 'startup', label: 'startup' },
      { value: 'clear', label: 'clear' },
    ],
    values: '`startup` | `clear`',
    notes: 'What triggered the start; `clear` = /clear-style rotation.',
  },
  {
    name: 'serviceName',
    type: 'string',
    kind: 'text',
    placeholder: 'nano',
    values: 'e.g. `nano`',
    notes: 'Analytics only; distinct from `initialize.clientInfo.name`, which identifies the client for the Compliance Logs Platform.',
  },
]

export const ACCESS_PRESETS = [
  { value: 'read-only', label: 'Read Only', approvalPolicy: 'on-request', sandbox: 'read-only' },
  { value: 'auto', label: 'Auto', approvalPolicy: 'on-request', sandbox: 'workspace-write' },
  { value: 'full-access', label: 'Full Access', approvalPolicy: 'never', sandbox: 'danger-full-access' },
]

// Classify an (approvalPolicy, sandbox) pair: '' when both unset, a preset
// value when the pair matches one, or 'custom' when set but matching none.
export function matchAccessPreset(approvalPolicy, sandbox) {
  if (!approvalPolicy && !sandbox) return ''
  const hit = ACCESS_PRESETS.find((p) => p.approvalPolicy === approvalPolicy && p.sandbox === sandbox)
  return hit ? hit.value : 'custom'
}

// Fresh per-param state: no default value, show flag from the initial set.
export function defaultSettings() {
  const out = {}
  for (const p of PARAMS) out[p.name] = { default: INITIAL_DEFAULTS[p.name] ?? '', show: INITIAL_SHOW.has(p.name) }
  return out
}

export function loadSettings() {
  const base = defaultSettings()
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (!raw) return base
    const stored = JSON.parse(raw)
    for (const p of PARAMS) {
      const s = stored?.[p.name]
      if (!s) continue
      if (typeof s.default === 'string') base[p.name].default = s.default
      if (typeof s.show === 'boolean') base[p.name].show = s.show
    }
  } catch {
    // corrupt/unavailable storage → fall back to defaults
  }
  return base
}

export function saveSettings(settings) {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(settings))
  } catch {
    // storage unavailable (private mode, quota) → silently skip
  }
}
