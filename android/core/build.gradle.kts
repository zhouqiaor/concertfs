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

    testOptions {
        // JVM CIO 真实 HTTP 集成测试覆盖 FsLog 分支，android.util.Log 使用 Android 默认桩。
        unitTests.isReturnDefaultValues = true
    }
}

kotlin {
    compilerOptions {
        jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_17)
    }
}

dependencies {
    api(libs.ktor.server.core)
    // CIO 仅用于环回 HTTP；公网 TLS 由 LoopbackTlsFront 承担。
    api(libs.ktor.server.cio)
    api(libs.ktor.server.content.negotiation)
    api(libs.ktor.server.status.pages)
    api(libs.ktor.serialization.kotlinx.json)
    api(libs.conscrypt.android)
    api(libs.bcpkix.jdk18on)

    testImplementation("junit:junit:4.13.2")
    testImplementation(libs.ktor.server.test.host)
}
