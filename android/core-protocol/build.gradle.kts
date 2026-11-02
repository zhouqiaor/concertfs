plugins {
    alias(libs.plugins.android.library)
    id("org.jetbrains.kotlin.plugin.serialization")
}

android {
    namespace = "com.phonefs.core.protocol"
    compileSdk = 36

    defaultConfig {
        minSdk = 26
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    sourceSets {
        getByName("main") {
            // 契约 codegen 生成物（projects/phonefs/scripts/gen-contract.*）。
            kotlin.srcDir("src/generated/kotlin")
        }
    }
}

kotlin {
    compilerOptions {
        jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_17)
    }
}

dependencies {
    // 纯协议层：只提供 kotlinx.serialization-json 与 ktor-http（HttpStatusCode）。
    // 不引入 Ktor server / Conscrypt / BouncyCastle。
    api(libs.ktor.serialization.kotlinx.json)

    testImplementation("junit:junit:4.13.2")
}
