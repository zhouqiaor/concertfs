plugins {
    alias(libs.plugins.android.application)
}

android {
    namespace = "com.phonefs.source"
    compileSdk = 36

    defaultConfig {
        applicationId = "com.phonefs.source"
        minSdk = 26
        targetSdk = 36
        // AGP 9 新 DSL：version 只在 defaultConfig（extension 顶层版本属性已移除）
        versionCode = 1
        versionName = "0.1.0-w2"
    }

    buildFeatures {
        buildConfig = true
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    buildTypes {
        debug {
            // W2：debug 签名即可（SPEC §12.4）
        }
    }

    packaging {
        resources {
            excludes += setOf(
                "META-INF/DEPENDENCIES",
                "META-INF/LICENSE*",
                "META-INF/NOTICE*",
                "META-INF/*.kotlin_module",
                "META-INF/versions/**/previous.bin",
                // Netty 多 jar 重复的 jar index（纯打包元数据，无运行期价值）
                "META-INF/INDEX.LIST",
                // Netty 版本横幅（仅 Version 日志用）
                "META-INF/io.netty.versions.properties",
            )
            // 第三方许可证/NOTICE 多 jar 同名冲突：保留首份（About-ThirdParty 清单 M1 汇总）
            pickFirsts += setOf(
                "META-INF/license/**",
                "META-INF/LICENSE.txt",
                "META-INF/NOTICE.txt",
                "META-INF/ASL2.0",
            )
        }
    }
}

kotlin {
    compilerOptions {
        jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_17)
    }
}

dependencies {
    implementation(project(":core"))
}
