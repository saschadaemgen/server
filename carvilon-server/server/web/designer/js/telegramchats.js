// Telegram chat-picker data: the allowlisted chats from
// /a/designer/telegram/chats. Unlike the GPIO line list (static
// hardware, cached for the editor's lifetime), the allowlist is
// dynamic - the core flow is "message the bot, approve on /a/telegram,
// pick the chat here" - so every inspector open fetches fresh
// (single-flight; the list is tiny). No claim pool either: several
// blocks may target the same chat (doorbell AND alarm); the serializer
// keeps their channel refs unique via the #node-id slot.

let inflight = null;

export async function loadChats() {
  if (inflight) return inflight;
  inflight = fetch('telegram/chats', { credentials: 'same-origin' })
    .then(r => r.ok ? r.json() : { chats: [] })
    .then(d => Array.isArray(d.chats) ? d.chats : [])
    .catch(() => [])
    .then(chats => { inflight = null; return chats; });
  return inflight;
}
