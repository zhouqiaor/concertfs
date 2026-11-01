package com.phonefs.source

import android.app.Application
import android.util.Log
import com.phonefs.core.crypto.IdentityStore
import com.phonefs.core.telemetry.FsLog
import org.conscrypt.Conscrypt
import java.security.Security

class PhoneFsApp : Application() {

    lateinit var store: IdentityStore
        private set

    override fun onCreate() {
        super.onCreate()
        FsLog.init(filesDir)
        // §7.3：Conscrypt 置 Provider 首位，保证 API26–28 亦有 TLS1.3。
        // W2 真机 A/B 闸门：外存标记文件 PhoneFS-w2/DISABLE_BUNDLED_CONSCRYPT 存在时
        // 跳过内置 Conscrypt 2.8.0，改用 ROM 平台 JSSE（API31 自带 TLS1.3），
        // 用于定位 TLS 拉流内存暴涨归因（内置 provider vs HTTP 管线）。
        val marker = java.io.File(android.os.Environment.getExternalStorageDirectory(),
            "PhoneFS-w2/DISABLE_BUNDLED_CONSCRYPT")
        val useBundled = !marker.exists()
        val inserted = if (useBundled) {
            try {
                Security.insertProviderAt(Conscrypt.newProvider(), 1) == 1
            } catch (t: Throwable) {
                Log.e(TAG, "Conscrypt init failed", t)
                false
            }
        } else false
        FsLog.i(TAG, "Conscrypt bundled=$useBundled inserted=$inserted providers=" +
            Security.getProviders().joinToString(",") { it.name })
        store = IdentityStore(this)
    }

    companion object {
        const val TAG = "phonefs.app"
    }
}
