// mqtt-ws.js — a minimal, first-party MQTT 3.1.1 client over WebSocket
// for the in-editor console. Local-first: no external dependency, no
// bundler, no CDN. It implements exactly what the console needs —
// CONNECT (with username/password), SUBSCRIBE, PUBLISH, incoming
// PUBLISH, keepalive PING, DISCONNECT — and nothing else.
//
// Usage:
//   const c = mqttConnect(url, {username, password, clientId});
//   c.on('connect', () => c.subscribe('#', 0));
//   c.on('message', (topic, payload, meta) => …);  // payload: Uint8Array
//   c.on('close', () => …); c.on('error', (e) => …);
//   c.publish(topic, str, {qos, retain}); c.end();
//
// The WebSocket subprotocol is "mqtt" (required by brokers). Binary
// frames carry MQTT packets; a streaming parser reassembles packets
// that WS framing may split or coalesce.

const PKT = { CONNECT: 1, CONNACK: 2, PUBLISH: 3, PUBACK: 4, SUBSCRIBE: 8, SUBACK: 9, PINGREQ: 12, PINGRESP: 13, DISCONNECT: 14 };

function encLen(n) {
  // MQTT remaining-length: 1–4 bytes, 7 bits each, high bit = continuation.
  const out = [];
  do {
    let d = n % 128;
    n = Math.floor(n / 128);
    if (n > 0) d |= 0x80;
    out.push(d);
  } while (n > 0);
  return out;
}

function encStr(s) {
  const b = new TextEncoder().encode(s);
  return [b.length >> 8, b.length & 0xff, ...b];
}

function frame(type, flags, body) {
  return new Uint8Array([(type << 4) | (flags & 0x0f), ...encLen(body.length), ...body]);
}

export function mqttConnect(url, opts = {}) {
  const handlers = { connect: [], message: [], close: [], error: [] };
  const emit = (ev, ...a) => handlers[ev].forEach(h => { try { h(...a); } catch (_) {} });

  let ws, pingTimer, alive = false, pid = 1, rx = new Uint8Array(0);
  const keepalive = 30;

  function send(bytes) { if (ws && ws.readyState === 1) ws.send(bytes); }
  function nextPid() { pid = (pid % 65535) + 1; return pid; }

  try {
    ws = new WebSocket(url, 'mqtt');
  } catch (e) {
    setTimeout(() => emit('error', e), 0);
    return api();
  }
  ws.binaryType = 'arraybuffer';

  ws.onopen = () => {
    const flags = 0x02 | (opts.username ? 0x80 : 0) | (opts.password ? 0x40 : 0); // clean session + creds
    const body = [
      ...encStr('MQTT'), 4, flags, keepalive >> 8, keepalive & 0xff,
      ...encStr(opts.clientId || ('cv-console-' + Math.floor(performance.now()))),
    ];
    if (opts.username) body.push(...encStr(opts.username));
    if (opts.password) body.push(...encStr(opts.password));
    send(frame(PKT.CONNECT, 0, body));
  };

  ws.onmessage = (ev) => {
    const chunk = new Uint8Array(ev.data);
    const merged = new Uint8Array(rx.length + chunk.length);
    merged.set(rx, 0); merged.set(chunk, rx.length);
    rx = merged;
    parse();
  };
  ws.onclose = () => { stopPing(); alive = false; emit('close'); };
  ws.onerror = (e) => emit('error', e);

  function parse() {
    // Pull every complete packet out of the rx buffer.
    for (;;) {
      if (rx.length < 2) return;
      // decode remaining length starting at byte 1
      let mult = 1, len = 0, i = 1, done = false;
      for (; i < rx.length && i <= 4; i++) {
        const b = rx[i];
        len += (b & 0x7f) * mult;
        mult *= 128;
        if ((b & 0x80) === 0) { done = true; i++; break; }
      }
      if (!done) return;          // length not fully arrived yet
      const total = i + len;
      if (rx.length < total) return; // body not fully arrived
      const type = rx[0] >> 4, flags = rx[0] & 0x0f;
      handlePacket(type, flags, rx.subarray(i, total));
      rx = rx.subarray(total);
    }
  }

  function handlePacket(type, flags, body) {
    switch (type) {
      case PKT.CONNACK: {
        const code = body.length >= 2 ? body[1] : 0xff;
        if (code === 0) { alive = true; startPing(); emit('connect'); }
        else { emit('error', new Error('CONNACK refused (code ' + code + ')')); try { ws.close(); } catch (_) {} }
        break;
      }
      case PKT.PUBLISH: {
        const qos = (flags >> 1) & 0x03, retain = (flags & 0x01) === 1;
        let p = 0;
        const tlen = (body[p] << 8) | body[p + 1]; p += 2;
        const topic = new TextDecoder().decode(body.subarray(p, p + tlen)); p += tlen;
        if (qos > 0) p += 2; // packet id (we ack QoS1 below)
        const payload = body.subarray(p);
        emit('message', topic, payload, { qos, retain });
        if (qos === 1) {
          const id = (body[2 + tlen] << 8) | body[3 + tlen];
          send(frame(PKT.PUBACK, 0, [id >> 8, id & 0xff]));
        }
        break;
      }
      case PKT.PINGRESP: break;
      default: break; // SUBACK/PUBACK ignored
    }
  }

  function startPing() {
    stopPing();
    pingTimer = setInterval(() => send(frame(PKT.PINGREQ, 0, [])), keepalive * 1000 * 0.8);
  }
  function stopPing() { if (pingTimer) { clearInterval(pingTimer); pingTimer = null; } }

  function api() {
    return {
      on(ev, h) { if (handlers[ev]) handlers[ev].push(h); return this; },
      connected() { return alive; },
      subscribe(filter, qos = 0) {
        const id = nextPid();
        send(frame(PKT.SUBSCRIBE, 0x02, [id >> 8, id & 0xff, ...encStr(filter), qos & 0x03]));
      },
      publish(topic, message, o = {}) {
        const qos = (o.qos || 0) & 0x03, retain = o.retain ? 0x01 : 0;
        const payload = typeof message === 'string' ? new TextEncoder().encode(message) : (message || new Uint8Array(0));
        const head = encStr(topic);
        let body;
        if (qos > 0) {
          const id = nextPid();
          body = [...head, id >> 8, id & 0xff, ...payload];
        } else {
          body = [...head, ...payload];
        }
        send(frame(PKT.PUBLISH, (qos << 1) | retain, body));
      },
      end() {
        try { send(frame(PKT.DISCONNECT, 0, [])); } catch (_) {}
        stopPing();
        try { ws && ws.close(); } catch (_) {}
      },
    };
  }
  return api();
}
