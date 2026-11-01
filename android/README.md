# PhoneFS Android（M0 W2 spike）

`:core` 协议层（Ktor HTTPS server + Conscrypt TLS1.3 + MES FsBackend）与 `:source` App（FGS + 配对/共享 UI）。
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

依赖：Ktor server-core / server-netty / content-negotiation / serialization-kotlinx-json、Conscrypt、bcpkix-jdk18on。
**未引入** OkHttp（W2 无客户端需求，YAGNI）、Room（M1 索引才引入）、AndroidX（UI 用框架控件，零额外依赖）。

## W2 关键变更：CIO → Netty（F-W2-1，待 ADR）

SPEC §2.2 钉「Ktor 3.6.1 CIO engine + TLS」。实测 Ktor 3.6.1 的 CIO 服务端引擎在 JVM 上
**不支持 HTTPS**：`CIOApplicationEngine.initServerJob` 对 `ConnectorType.HTTPS` 显式抛
`UnsupportedOperationException("CIO Engine does not currently support HTTPS…")`
（GitHub ktorio/ktor tag 3.6.1，ktor-server-cio/common/.../CIOApplicationEngine.kt 第 223–228 行；
KTOR-825 长期开放）。

W2 改用同一 Ktor 版本的 **Netty 引擎**（`io.ktor:ktor-server-netty:3.6.1`）：协程模型、content-negotiation、
SSE 插件生态全部不变；Netty 走 JDK SSL（未带 netty-tcnative）→ provider 顺序首位 Conscrypt 出 TLS1.3，
`EngineSSLConnectorBuilder.enabledProtocols=["TLSv1.3"]` 强制禁降级。SPEC §2.2/ADR-16 需补 ADR。

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
