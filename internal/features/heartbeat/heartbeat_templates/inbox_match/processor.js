const fs = require('node:fs');

// Settings are optional. With no sender and no reference, any unread message
// matches. Unknown keys and non-string values are configuration errors, so a
// mistyped filter cannot silently widen into "any unread message".
const SETTING_KEYS = new Set(['senderHostedThreadId', 'refKind', 'refId']);
const PAGE_LIMIT = 100;
const MAX_PAGES = 50;

function finish(value) {
  process.stdout.write(JSON.stringify(value));
}

class Failure extends Error {
  constructor(reason) {
    super(reason);
    this.reason = reason;
  }
}

function readSettings(settings) {
  if (!settings || typeof settings !== 'object' || Array.isArray(settings)) throw new Failure('settings_invalid');
  for (const [key, value] of Object.entries(settings)) {
    if (!SETTING_KEYS.has(key) || typeof value !== 'string') throw new Failure('settings_invalid');
  }
  const sender = settings.senderHostedThreadId || '';
  const refKind = settings.refKind || '';
  const refId = settings.refId || '';
  if ((refKind && !refId) || (!refKind && refId)) throw new Failure('settings_invalid');
  return {sender, refKind, refId};
}

(async () => {
  try {
    let input;
    try {
      input = JSON.parse(fs.readFileSync(0, 'utf8'));
    } catch (_) {
      throw new Failure('input_invalid');
    }
    if (!input || input.version !== 1 || typeof input.targetHostedThreadId !== 'string') throw new Failure('input_invalid');
    const {sender, refKind, refId} = readSettings(input.settings);
    const origin = process.env.NANO_API_ORIGIN || 'http://127.0.0.1:7690';
    const target = encodeURIComponent(input.targetHostedThreadId);
    let since = 0;
    for (let page = 0; page < MAX_PAGES; page += 1) {
      const query = new URLSearchParams({actorThreadRef: input.targetHostedThreadId, status: 'unread', peek: 'true', since: String(since), limit: String(PAGE_LIMIT)});
      const response = await fetch(`${origin}/api/v1/hosted-threads/${target}/inbox?${query}`);
      if (!response.ok) throw new Failure('inbox_read_failed');
      const value = await response.json();
      if (!value || !Array.isArray(value.items)) throw new Failure('inbox_read_failed');
      const previousSince = since;
      for (const item of value.items) {
        if (!item || item.status !== 'unread' || !item.sender || !Array.isArray(item.refs) || !Number.isSafeInteger(item.seq)) {
          throw new Failure('inbox_read_failed');
        }
        const senderMatches = !sender || item.sender.hostedThreadId === sender;
        const refMatches = !refKind || item.refs.some((ref) => ref && ref.kind === refKind && ref.id === refId);
        if (senderMatches && refMatches) {
          finish({decision: 'wake', reason: 'inbox_match'});
          return;
        }
        if (item.seq > since) since = item.seq;
      }
      if (value.items.length < PAGE_LIMIT) {
        finish({decision: 'skip', reason: 'inbox_no_match'});
        return;
      }
      if (since <= previousSince) throw new Failure('inbox_read_failed');
    }
    throw new Failure('inbox_scan_limit');
  } catch (error) {
    finish({decision: 'fail', reason: error instanceof Failure ? error.reason : 'inbox_read_failed'});
  }
})();
