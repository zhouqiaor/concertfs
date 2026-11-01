package com.phonefs.source

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.bluetooth.BluetoothAdapter
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.net.wifi.WifiManager
import android.os.Build
import android.os.IBinder
import android.provider.Settings
import com.phonefs.core.telemetry.FsLog
import com.phonefs.core.fs.MesFsBackend
import com.phonefs.core.net.PhoneFsServer
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking

class SharingService : Service() {

    private val scope = CoroutineScope(Job() + Dispatchers.IO)
    private var server: PhoneFsServer? = null
    private var heartbeat: Job? = null
    private var wifiLock: WifiManager.WifiLock? = null
    @Volatile private var port: Int = -1
    private val starting = java.util.concurrent.atomic.AtomicBoolean(false)

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            FsLog.i(TAG, "stop requested from notification")
            stopSharing()
            return START_NOT_STICKY // ADR-11：不做 sticky 死循环
        }
        startInForeground("PhoneFS 启动中…")
        scope.launch { startServer() }
        startHeartbeat()
        acquireWifiLock()
        return START_NOT_STICKY
    }

    private fun startInForeground(text: String) {
        val n = buildNotification(text)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            startForeground(NOTIF_ID, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_CONNECTED_DEVICE)
        } else {
            startForeground(NOTIF_ID, n)
        }
    }

    private suspend fun startServer() {
        if (!starting.compareAndSet(false, true)) {
            FsLog.i(TAG, "start ignored: server already running/starting port=$port")
            return
        }
        try {
            val app = application as PhoneFsApp
            val identity = app.store.loadOrCreateIdentity()
            val token = app.store.loadOrCreateToken()
            val deviceId = app.store.loadOrCreateDeviceId()
            val srv = PhoneFsServer(
                backend = MesFsBackend(),
                identity = identity,
                deviceId = deviceId,
                deviceName = resolveDeviceName(),
                token = token,
            )
            port = srv.start(PREFERRED_PORT)
            server = srv
            val fixture = runCatching { FixtureMaker.ensure() }.getOrNull()
            starting.set(false)
            FsLog.i(TAG, "server started port=$port ips=${NetInfo.siteLocalIpv4()} fp=${identity.fingerprint}")
            FsLog.i(TAG, "fixture=${fixture?.absolutePath} mes=${android.os.Environment.isExternalStorageManager()}")
            val nm = getSystemService(NotificationManager::class.java)
            nm.notify(NOTIF_ID, buildNotification("PhoneFS 共享中 · 端口 $port"))
        } catch (t: Throwable) {
            starting.set(false)
            FsLog.e(TAG, "server start failed", t)
            stopSelf()
        }
    }

    /** 对标华为 HMDFS 卷标：使用系统「设置-设备名称」（softbus 同源），不是品牌+型号拼接。 */
    private fun resolveDeviceName(): String {
        val systemName = runCatching {
            Settings.Global.getString(contentResolver, "device_name")
        }.getOrNull()?.trim()
        if (!systemName.isNullOrEmpty()) return systemName

        // 无蓝牙适配器或未持 BLUETOOTH_CONNECT 时不得影响共享服务启动。
        @Suppress("DEPRECATION")
        val bluetoothName = runCatching {
            BluetoothAdapter.getDefaultAdapter()?.name
        }.getOrNull()?.trim()
        if (!bluetoothName.isNullOrEmpty()) return bluetoothName

        return Build.MODEL.orEmpty().trim().ifEmpty { "Android" }
    }

    private fun startHeartbeat() {
        heartbeat = scope.launch {
            while (isActive) {
                delay(HEARTBEAT_MS)
                FsLog.i(
                    "phonefs.audit",
                    "alive ts=${System.currentTimeMillis()} port=$port pid=${android.os.Process.myPid()} ips=${NetInfo.siteLocalIpv4()}",
                )
            }
        }
    }

    @Suppress("DEPRECATION")
    private fun acquireWifiLock() {
        val wm = applicationContext.getSystemService(Context.WIFI_SERVICE) as WifiManager
        wifiLock = wm.createWifiLock(WifiManager.WIFI_MODE_FULL_HIGH_PERF, "phonefs:sharing").apply {
            setReferenceCounted(false)
            acquire()
        }
    }

    private fun stopSharing() {
        heartbeat?.cancel()
        wifiLock?.let { runCatching { if (it.isHeld) it.release() } }
        runBlocking(Dispatchers.IO) { runCatching { server?.stop() } }
        stopForeground(STOP_FOREGROUND_REMOVE)
        stopSelf()
    }

    override fun onDestroy() {
        runCatching {
            if (wifiLock?.isHeld == true) wifiLock?.release()
            server?.stop()
        }
        scope.coroutineContext[kotlinx.coroutines.Job]?.cancel()
        super.onDestroy()
    }

    private fun buildNotification(text: String): Notification {
        val nm = getSystemService(NotificationManager::class.java)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O && nm.getNotificationChannel(CH_ID) == null) {
            nm.createNotificationChannel(
                NotificationChannel(CH_ID, "PhoneFS 共享", NotificationManager.IMPORTANCE_LOW),
            )
        }
        val open = PendingIntent.getActivity(
            this, 0, Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val stop = PendingIntent.getService(
            this, 1, Intent(this, SharingService::class.java).setAction(ACTION_STOP),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        return Notification.Builder(this, CH_ID)
            .setContentTitle("PhoneFS-Source")
            .setContentText(text)
            .setSmallIcon(android.R.drawable.stat_sys_upload)
            .setOngoing(true)
            .setContentIntent(open)
            .addAction(android.R.drawable.ic_media_pause, "停止共享", stop)
            .build()
    }

    companion object {
        const val ACTION_STOP = "com.phonefs.source.STOP"
        private const val TAG = "phonefs.svc"
        private const val NOTIF_ID = 1001
        private const val CH_ID = "phonefs-sharing"
        private const val HEARTBEAT_MS = 15_000L
        private const val PREFERRED_PORT = 18443

        fun start(ctx: Context) {
            val i = Intent(ctx, SharingService::class.java)
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) ctx.startForegroundService(i) else ctx.startService(i)
        }
    }
}
