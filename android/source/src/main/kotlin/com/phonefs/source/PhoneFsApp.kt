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
        // §7.3：Conscrypt 置 Provider 首位，保证 API26–28 亦有 TLS1.3（本机 API31 亦经此路）
        val inserted = try {
            Security.insertProviderAt(Conscrypt.newProvider(), 1) == 1
        } catch (t: Throwable) {
            Log.e(TAG, "Conscrypt init failed", t)
            false
        }
        FsLog.i(TAG, "Conscrypt provider inserted=$inserted provider=${Security.getProviders()[0]}")
        store = IdentityStore(this)
    }

    companion object {
        const val TAG = "phonefs.app"
    }
}
