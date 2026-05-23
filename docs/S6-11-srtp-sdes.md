# S6-11: SRTP/SDES decryption for UniFi `?enableSrtp` streams

## Background — what changed since S1

The S1-07 evaluation concluded SRTP was infeasible because UniFi was
believed to use MIKEY for key exchange and the Go ecosystem had no
working MIKEY path. The **MIKEY-RE-Chat verified in late 2026 that
UniFi does NOT use MIKEY** — it uses SDES (RFC 4568). The SRTP master
key sits in cleartext in the SDP via `a=crypto:inline:<base64>|...`.
No key exchange, no Diffie-Hellman, no asymmetric crypto required.
What was a research blocker is now a portage task.

The verified reference lives in `C:\Projects\UniFi\tools\mikey_crack\`
as Python scripts; this commit ports the relevant slice (the
`SrtpReceiver` class from `11_full_decoder.py`) into stdlib Go.

## What S6-11 builds

`internal/source/unifi/srtp.go`:

- **`srtpKDF`** — RFC 3711 §4.3.1 key-derivation function. Builds the
  per-session IV (`master_salt` XOR `label << 48`, padded to 16 B),
  then returns the first `outLen` bytes of the AES-128-CTR keystream.
  Verified against the **RFC 3711 Appendix B.3** test vectors for all
  three labels (encryption, authentication, salt) in
  `TestSRTPKDF_RFC3711AppendixB3`.

- **`aesCMKeystream`** — thin wrapper over `crypto/cipher.NewCTR`.
  No third-party SRTP library: pion/srtp / libsrtp reject packets
  larger than ~2 KiB (`packet is too long`), a compile-time C limit
  that UniFi's intra-frame TCP-interleaved packets (up to ~65 KiB)
  routinely exceed. AES-CM is a stream cipher; size is not a
  cryptographic concern.

- **`extractSDESVideoKey`** — scans the SDP body for the video
  section's `a=crypto:` inline payload, base64-decodes the
  pre-`|`-pipe part, and asserts the standard 30-byte shape
  (16 B master key + 14 B master salt). Lifetime / MKI metadata is
  ignored — we use one key per session, no rotation.

- **`srtpReceiver`** — stateful per-SSRC receiver. Holds the three
  derived session keys (encryption, authentication, salt) plus the
  ROC (32-bit roll-over counter) and the last seen sequence number.
  Public method `process(packet []byte)` takes the full SRTP-on-the-
  wire packet (RTP header + encrypted body + 10-byte tag) and
  returns the cleartext H.264 payload or one of `ErrSRTPAuth` /
  `ErrSRTPMalformed`.

  - **ROC update** follows the simplified rule for ordered transports
    (RFC 3711 §3.3.1 sketch): if the sequence number jumps backwards
    by more than `0x8000`, treat it as a 16-bit wrap and bump ROC.
    Verified against a synthetic double-wrap in
    `TestSRTPReceiver_ROCWrap`.

  - **Per-packet IV** is built per RFC 3711 §4.1.1:
    `IV = session_salt(112b, 0-padded to 128b)
          XOR (ssrc(32b) << 64)
          XOR (packet_index(48b) << 16)`,
    with `packet_index = ROC << 16 | seq`. The byte-position
    accounting (ssrc into bytes 4..7, packet_index into bytes 8..13)
    follows the Python prototype byte-for-byte.

  - **HMAC verification** computes `HMAC-SHA1(authKey, packet[:len-10]
    || ROC_BE)[:10]` and constant-time-compares against the trailing
    10 bytes. Failed packets are rejected (cleartext is never
    returned for an unauthenticated input).

## Integration into `unifi.Source`

`internal/source/unifi/unifi.go`:

- `NewSource` no longer rejects `EncryptionSRTP`. The sentinel
  `ErrEncryptionSRTPNotImplemented` is kept exported as deprecated
  for any external `errors.Is` check, but is never returned now.

- `Source.srtp *srtpReceiver` field. Nil in `EncryptionTLS` mode.

- `Source.Start` skips the `stripEnableSrtp` URL massage in SRTP
  mode (the camera needs the query param to enable SRTP). After the
  RTSP `DESCRIBE` succeeds, the SDP body is fed to
  `extractSDESVideoKey`; the 30-byte secret is split (16 B + 14 B)
  and handed to `newSRTPReceiver`. The master key is **wiped from
  the heap** (`for i := range keysalt { keysalt[i] = 0 }`) once the
  session keys are derived — best-effort defence against post-
  mortem memory reading.

- `Source.handlePacket` calls `pkt.Marshal()` to reconstruct the
  wire bytes, passes them through `srtp.process`, and feeds the
  returned cleartext to the existing H.264 depacketizer. The TLS
  path is byte-identical to before; only the SRTP path is new.

- **Logging discipline**: the master key, the master salt, the
  derived session keys, the per-packet IVs, and the auth tags are
  NEVER written to the log. The SDES key extraction logs only
  "SRTP receiver armed (session keys derived; master key wiped
  from heap)"; the existing SDP-introspection log (`sdp.go`)
  already redacts inline keys per S1-06.

## Control interface — for the future admin switch

Today the Encryption mode flows through:

```
cmd/spike/main.go:
   encryption := os.Getenv("UNIFI_ENCRYPTION")
        ↓
   srcFactory → unifi.NewSource(unifi.Options{Encryption: unifi.Encryption(encryption), ...})
        ↓
   unifi.Source.opts.Encryption    ← THE control point
```

`unifi.Options.Encryption` (type `unifi.Encryption`, values
`unifi.EncryptionTLS` / `unifi.EncryptionSRTP`) is the **single
source of truth**. The env var is just one way the spike populates
it.

To move the switch into the admin UI later:

1. Add an `Encryption string` field to `profile.Profile` (one of
   `"tls"` / `"srtp"`, empty = default `"tls"`).
2. Have the `SourceFactory` in `cmd/spike/main.go` (or the carvilon-
   side equivalent) read it from the profile that triggered the
   subscribe, instead of from the env var.
3. The HTTP `PUT /api/profiles/{name}` body grows the field; the
   GET response surfaces it; the admin UI renders a checkbox.

S6-11 deliberately does NOT do that change — the briefing scoped it
as a separate piece of work. The internal control point is named
explicitly here so the master-chat can plan against it.

## Tests

- `TestSRTPKDF_RFC3711AppendixB3` — RFC 3711 Appendix B.3 test
  vectors for all three labels. Hex-anchored, no fuzz.
- `TestSRTPKDF_RejectsBadKeySize` — guards against silent garbage
  when a caller passes the wrong-size key or salt buffer.
- `TestSRTPReceiver_ROCWrap` — synthetic double-wrap; asserts ROC
  goes `0 → 1 → 2` at the right packet boundaries.
- `TestSRTPReceiver_NoWrapOnSmallBackwardJump` — a small backwards
  step (reorder, not wrap) must NOT bump ROC.
- `TestSRTPProcess_RoundTrip` — round-trip a fake packet through
  encrypt-and-tag → `srtpReceiver.process` → cleartext. With the
  KDF nailed independently, a passing round-trip proves the IV /
  XOR / HMAC pipeline is correct.
- `TestSRTPProcess_AuthFailureRejected` — bit-flipped tag returns
  `ErrSRTPAuth`; the cleartext from such a packet is NEVER
  returned to the depacketizer.
- `TestSRTPProcess_RoundTripAcrossWrap` — pre-wrap (ROC=0) and
  post-wrap (ROC=1) packets both decrypt+verify correctly,
  proving the ROC value the receiver applies actually matches
  what was used to encrypt.
- `TestExtractSDESVideoKey_*` — SDP parsing: picks the video
  section's key (not audio's), errors on a missing crypto line,
  rejects wrong-size payloads.
- `TestNewSource_EncryptionSRTPAccepted` (renamed from
  `_EncryptionSRTPNotImplemented`) — locks in the new behaviour:
  NewSource accepts the mode, the receiver is NOT armed yet at
  construction (only at Start, once the SDP is on hand).
- All existing TLS-mode tests still pass; the TLS path is unchanged.

`-race` clean. `GOOS=linux GOARCH=arm64` cross-compile clean —
the production RPi target builds cleanly.

## Scope explicitly NOT done

- **Audio (AAC / Opus) tracks 0/1** — identical SDES mechanism on a
  different SSRC, different depacketizer. Add when audio enters the
  pipeline.
- **RTCP / SRTCP** — separate index, separate IV layout. We don't
  decrypt the RTCP control channel; gortsplib's own RTCP handling
  is fine as-is.
- **MKI / key rotation** — UniFi advertises a Master Key Identifier
  field in the inline string but in practice ships one key per
  session. If the camera ever rotates mid-session we'll need to
  parse MKI and switch keys. Today: not observed.
- **Admin switch** — the briefing explicitly defers the carvilon-
  admin UI work. `unifi.Options.Encryption` is the named control
  point for it.

## Live verification on the UDM

```sh
# Spike with SRTP turned on:
export UNIFI_NVR_HOST=192.168.1.42
export UNIFI_API_KEY=<protect-integration-key>
export UNIFI_ENCRYPTION=srtp
go run ./cmd/spike

# Expected log lines on first viewer connect:
#   unifi: encryption mode=srtp
#   unifi: SDP security: ... a=crypto count=3 {tag=1 suite=AES_CM_128_HMAC_SHA1_80 inline=<redacted 30B> ...
#   unifi: SRTP receiver armed (session keys derived; master key wiped from heap)
#   unifi: first IDR received, starting access-unit output

# Browser test page → connect mjpeg_bal / intercom_web; image must
# flow exactly as in TLS mode (no visual difference; the security
# layer is on the wire only).

# Cross-check the H.264 output with the Python reference:
#   python3 C:/Projects/UniFi/tools/mikey_crack/11_full_decoder.py \
#       'rtsps://192.168.1.42:7441/<token>?enableSrtp'
# Both pipelines should report HMAC-clean packets and the same H.264
# stream shape (SPS+PPS+IDR at GoP boundaries, plausible frame size
# and timing).
```
