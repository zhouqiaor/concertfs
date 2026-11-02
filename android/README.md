# PhoneFS Android（M0 W2 spike）

`:core-protocol` 纯协议层（DTO / ErrCode / DocIds / codegen 模型，无 Ktor server 依赖）→ `:core-server` 服务端（Ktor HTTPS server + Conscrypt TLS1.3 + MES FsBackend）→ `:source` App（FGS + 配对/共享 UI）。依赖方向：`core-server → core-protocol`，`source → core-server`。
W2 退出标准见 `docs/design/2026-10-31-phone-files-selfbuild.md` §13，闸门报告：
`docs/research/2026-10-31-phonefs-m0-w2-android-ktor-gate.md`。

## 版本选型与下载证据（2026-10-31 联网核实）

| 组件 | 钉版 | 证据 |
|---|---|---|
| Gradle | 9.8.2（2026-10-19 GA） | https://services.gradle.org/versions/current 返回 9.8.2，sha256 21bd5da2…；wrapper 走 services.gradle.org/distributions |
| AGP | 9.5.0 | dl.google.com/android/maven2 `com.android.tools.build:gradle` maven-metadata，9.x 最新稳定 9.5.0 |
| Kotlin | 2.4.21 | Maven Central `org.jetbrains.kotlin:kotlin-gradle-plugin` 最新非 Beta 稳定版（2.5.0-Beta2 为 beta，不选） |
| Ktor | 3.6.1 | SPEC §2.2 钉版；Maven Central latest=release=3.6.1 |
| Conscrypt | 2.8.0 | Maven Central `org.conscrypt:conscrypt-android` 最新稳定（2.7-alpha 不取） |
| BouncyCastle | bcpkix-jdk18on 1.86 | Maven Central latest=1.86，仅用于自签 X.509 构建（SPEC §2.2） |
| compileSdk / target | 36 | SPEC 冻结；minSdk 26；本机 platform android-36、build-tools 36/37 已装 |

依赖：Ktor server-core / server-cio / content-negotiation / serialization-kotlinx-json、Conscrypt、bcpkix-jdk18on。
**未引入** OkHttp（W2 无客户端需求，YAGNI）、Room（M1 索引才引入）、AndroidX（UI 用框架控件，零额外依赖）。

## 引擎沿革：CIO → Netty → CIO（ADR-19 增补，2026-11-01）

SPEC §2.2 钉「Ktor 3.6.1 CIO engine + TLS」。W2 发现 Ktor 3.6.1 的 CIO 服务端引擎在 JVM 上
**不支持 HTTPS**：`CIOApplicationEngine.initServerJob` 对 `ConnectorType.HTTPS` 显式抛
`UnsupportedOperationException("CIO Engine does not currently support HTTPS…")`
（KTOR-825 长期开放），于是 W2 切 **Netty**（`ktor-server-netty:3.6.1`，JDK SSL + Conscrypt，
`enabledProtocols=["TLSv1.3"]`）并写进 ADR-19。

M3 硬化时 Netty 在 >2GiB 大文件 GET 上 RSS 涨到 4.74–7.29GiB 被系统杀；SPEC ADR-19 增补把生产引擎
**回切 CIO**：TLS 由 `LoopbackTlsFront`（Conscrypt `SSLServerSocket`）对外承担，CIO 只服务
`127.0.0.1` 明文，原「CIO 不支持 HTTPS」约束不再适用。真机 2,147,495,993B 全量 GET 内存恒定
（VmHWM ~188MiB），W2/M2/M3/rw/ro 回归全绿。详见 SPEC ADR-19 增补与 `test/m3/m3-report.md`。

## 构建

```bash
# local.properties（不入库）
echo 'sdk.dir=C\:\\Programs\\Android\\Sdk' > local.properties

./gradlew :source:assembleDebug
# APK: source/build/outputs/apk/debug/source-debug.apk
```

JDK 21（`java -version` = 21.0.11）。无需 NDK。

## W2 简化项（相对 SPEC）

见闸门报告「简化项清单」。要点：预共享 Bearer（App 屏展示，非 SAS/ECDH 配对）、
身份 PEM 落 filesDir 0700（未做 Keystore vault）、list 无 cursor 分页、cat:* 返回 501（index M1）、
无 mDNS/NSD、无 SSE、DTO 手写（codegen 管道随契约 yaml 在 W3/M0 D1 前置补）。
