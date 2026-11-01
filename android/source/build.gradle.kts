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

    signingConfigs {
        create("w2DebugKey") {
            val home = System.getProperty("user.home")
            storeFile = File(home, ".android/debug.keystore")
            storePassword = "android"
            keyAlias = "androiddebugkey"
            keyPassword = "android"
        }
    }

    buildTypes {
        debug {
            // W2：debug 签名即可（SPEC §12.4）
        }
        release {
            // W2 真机闸门专用：非 debuggable 包用于 A/B 验证「CheckJNI（debuggable 进程
            // 默认开启 -Xcheck:jni）下 Conscrypt JNI 记账导致 LinearAlloc 暴涨」假设。
            // 复用本机标准 debug 密钥（口令 android 是 AOSP 公开默认值，非密钥），
            // 使 release 包可在已装 debug 包上 -r 覆盖安装；不开 minify（M1 再议）。
            signingConfig = signingConfigs.getByName("w2DebugKey")
            isMinifyEnabled = false
            isDebuggable = false
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
