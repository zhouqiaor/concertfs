package com.phonefs.source

import android.Manifest
import android.app.Activity
import android.content.Intent
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.Environment
import android.provider.Settings
import android.security.NetworkSecurityPolicy
import android.view.Gravity
import android.view.ViewGroup
import android.widget.Button
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView
import java.security.Security

class MainActivity : Activity() {

    private lateinit var status: TextView

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        if (Build.VERSION.SDK_INT >= 33) {
            requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), 7)
        }
        val pad = (resources.displayMetrics.density * 18).toInt()
        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(pad, pad, pad, pad)
        }
        val title = TextView(this).apply { textSize = 20f; text = "PhoneFS-Source（W2 spike）" }
        status = TextView(this).apply { textSize = 13f; setTextIsSelectable(true) }
        val scroll = ScrollView(this).apply {
            addView(status, ViewGroup.LayoutParams(-1, -2))
        }
        fun btn(label: String, action: () -> Unit) = Button(this).apply {
            text = label
            setOnClickListener { action(); refresh() }
        }
        val mes = btn("授予 / 检查「所有文件访问」(MES)") { openMesSettings() }
        val start = btn("启动共享（FGS + TLS server）") { SharingService.start(this) }
        val stop = btn("停止共享") {
            startService(Intent(this, SharingService::class.java).setAction(SharingService.ACTION_STOP))
        }
        root.addView(title)
        root.addView(mes)
        root.addView(start)
        root.addView(stop)
        root.addView(scroll, LinearLayout.LayoutParams(-1, 0, 1f))
        setContentView(root)
        root.gravity = Gravity.START
    }

    override fun onResume() {
        super.onResume()
        if (::status.isInitialized) refresh()
    }

    private fun refresh() {
        val app = application as PhoneFsApp
        val id = app.store.loadOrCreateIdentity()
        val token = app.store.loadOrCreateToken()
        val deviceId = app.store.loadOrCreateDeviceId()
        val provider0 = Security.getProviders()[0]
        val tlsProvider = runCatching {
            javax.net.ssl.SSLContext.getInstance("TLSv1.3").provider.name
        }.getOrNull()
        val ips = NetInfo.siteLocalIpv4()
        val sb = StringBuilder()
        sb.appendLine("设备：${Build.MANUFACTURER} ${Build.MODEL}（API ${Build.VERSION.SDK_INT}）")
        sb.appendLine("deviceId: $deviceId")
        sb.appendLine("Provider[0]: ${provider0.name} ${provider0.version}")
        sb.appendLine("TLSv1.3 Provider: $tlsProvider")
        sb.appendLine("cleartext permitted: ${NetworkSecurityPolicy.getInstance().isCleartextTrafficPermitted}")
        sb.appendLine("MES 已授权: ${Environment.isExternalStorageManager()}")
        sb.appendLine("站点本地 IPv4: ${ips.joinToString().ifEmpty { "（未连接 Wi-Fi LAN）" }}")
        sb.appendLine("首选端口: 18443（占用时动态选择，见 logcat tag phonefs.svc）")
        sb.appendLine()
        sb.appendLine("指纹 SHA-256:")
        sb.appendLine(id.fingerprint)
        sb.appendLine()
        sb.appendLine("Bearer token（W2 预共享演示配对）:")
        sb.appendLine(token)
        sb.appendLine()
        ips.forEach { sb.appendLine("https://$it:18443/v1/pair/info") }
        sb.appendLine()
        sb.appendLine("logcat：adb logcat -s phonefs.svc phonefs.audit phonefs.app phonefs.fixture")
        status.text = sb.toString()
    }

    private fun openMesSettings() {
        val intent = Intent(Settings.ACTION_MANAGE_APP_ALL_FILES_ACCESS_PERMISSION)
            .setData(Uri.parse("package:$packageName"))
            .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        runCatching { startActivity(intent) }
            .onFailure {
                startActivity(
                    Intent(Settings.ACTION_MANAGE_ALL_FILES_ACCESS_PERMISSION)
                        .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
                )
            }
    }
}
