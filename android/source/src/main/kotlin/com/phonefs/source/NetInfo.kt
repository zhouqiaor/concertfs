package com.phonefs.source

import java.net.Inet4Address
import java.net.NetworkInterface

object NetInfo {
    /** 枚举站点本地 IPv4（Wi-Fi LAN 联调用；排除 loopback/PPP）。 */
    fun siteLocalIpv4(): List<String> =
        runCatching {
            NetworkInterface.getNetworkInterfaces().toList()
                .filter { it.isUp && !it.isLoopback }
                .flatMap { it.inetAddresses.toList() }
                .filterIsInstance<Inet4Address>()
                .filter { it.isSiteLocalAddress }
                .map { it.hostAddress ?: "" }
                .filter { it.isNotEmpty() }
        }.getOrDefault(emptyList())
}
