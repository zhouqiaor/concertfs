// AGP 9.5 内置 Kotlin（默认 KGP 2.2.10）：按官方迁移指引 buildscript classpath 升到 2.4.21；
// 不再 apply org.jetbrains.kotlin.android（AGP 9 起内置，apply 反而报错）。
buildscript {
    dependencies {
        classpath("org.jetbrains.kotlin:kotlin-gradle-plugin:2.4.21")
        classpath("org.jetbrains.kotlin:kotlin-serialization:2.4.21")
    }
}

plugins {
    alias(libs.plugins.android.application) apply false
    alias(libs.plugins.android.library) apply false
}
