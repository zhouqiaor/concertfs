plugins {
    alias(libs.plugins.android.library)
    id("org.jetbrains.kotlin.plugin.serialization")
}

android {
    namespace = "com.phonefs.core"
    compileSdk = 36

    defaultConfig {
        minSdk = 26
        consumerProguardFiles("consumer-rules.pro")
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
}

kotlin {
    compilerOptions {
        jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_17)
    }
}

dependencies {
    api(libs.ktor.server.core)
    // W2 HTTP/1.1 only（ALPN 只 http/1.1）：排除 QUIC/HTTP3 原生 codec。
    // 注意：epoll/kqueue 的「Java 检测类」主 jar 不能排——Ktor NettyApplicationEngine
    // 选 channel 时直接引用 io.netty.channel.kqueue.KQueue / epoll.Epoll 类字面量，
    // 排掉会在启动时 NoClassDefFoundError（isAvailable()=false 后正常回退 NIO）。
    // 真正的 .so/.jnilib 在独立 classifier 构件（linux-x86_64/osx-*）里，Android 本就不拉。
    api(libs.ktor.server.netty) {
        exclude(group = "io.netty", module = "netty-codec-native-quic")
        exclude(group = "io.netty", module = "netty-codec-http3")
    }
    api(libs.ktor.server.content.negotiation)
    api(libs.ktor.server.status.pages)
    api(libs.ktor.serialization.kotlinx.json)
    api(libs.conscrypt.android)
    api(libs.bcpkix.jdk18on)
}
