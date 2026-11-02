# concertfs

**Multi-device file collaboration** — a cross-OS, cross-form-factor peer filesystem
that mounts a remote device's storage as a local drive.

> Status: early open-source release. The codebase internally still carries the
> `phonefs` name (this is a rename-in-progress; the public brand is `concertfs`).
> Both names refer to the same project.

## What it does

concertfs lets devices of different operating systems and form factors share and
collaborate over their files with each other as peers — no cloud, no account, no
master/slave. A phone, a PC, a tablet, or any other node can present its storage
to another node and have it appear as a local drive (read-only or read-write),
with the same typed HTTP contract on every side.

It is built as a capability-oriented, device-neutral system: like a distributed
file capability, remote resources behave as if local, without binding to any one
kind of device.

## Architecture (the "C 分家" split)

The two reference platforms diverge on implementation but converge on contract:

| Platform | Stack | Notes |
|---|---|---|
| **Windows** | Go (+ cgofuse/WinFsp, no-cgo) | mounts as a drive letter; TLS 1.3 front; loops back plaintext to a local HTTP engine |
| **Android** | Kotlin, native (zero JNI) | phone source app; pure-JVM protocol layer for consumer apps |

Both sides share **one OpenAPI contract** (`contract/`) and its generated models
(Go `oapi-codegen`, Kotlin `openapi-generator`). Hand-written business DTOs are
forbidden — the contract is the single source of truth.

### Discovery & security

- mDNS discovery, with QR-code / manual-IP fallback
- TLS 1.3 + bearer token
- Pairing via certificate fingerprint / short authentication string (SAS)
- Per-session read-only / read-write authorization

## Repository layout

```
android/      Kotlin source app + server + pure-JVM protocol layer
go/           Go server (same contract, nocgo virtual drive)
contract/     OpenAPI 3.1 contract + dual-side codegen
scripts/      codegen entry + tooling
test/         integration / regression harness
```

## Developer quick start

```bash
# regenerate contract models (zero-diff; deterministic)
cd scripts && bash gen-contract.sh

# Android (needs JDK 21, AGP 9)
cd android && ./gradlew :core-protocol:testDebugUnitTest :core-server:testDebugUnitTest :source:assembleDebug

# Go (needs Go 1.26)
cd go && go build ./... && go vet ./...
```

## License

[MIT](LICENSE) — © 2026 Qiao Zhou
